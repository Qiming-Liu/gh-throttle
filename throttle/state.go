package throttle

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	scopeAll   = "all"
	scopeWrite = "write"
	resPrefix  = "resource:"
)

type writeRecord struct {
	// At is when the write was admitted, in Unix nanoseconds.
	At int64 `json:"at"`
	// Hash identifies the write for deduplication. It is cleared when the
	// write did not succeed, so a failed write may be retried immediately.
	Hash string `json:"hash,omitempty"`
}

type breakerState struct {
	At     int64  `json:"at"`
	Reason string `json:"reason"`
}

// State is shared by every gh process through state.json, read and written
// only while state.lock is held.
type State struct {
	Writes []writeRecord `json:"writes"`
	// Cooldowns maps a scope ("all", "write" or "resource:<name>") to the Unix
	// nanosecond time until which requests in that scope are refused.
	Cooldowns map[string]int64 `json:"cooldowns"`
	// Strikes are the Unix nanosecond times of secondary rate limits.
	Strikes []int64       `json:"strikes"`
	Breaker *breakerState `json:"breaker,omitempty"`
}

func loadState(path string) (*State, error) {
	s := &State{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.Cooldowns = map[string]int64{}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, s); err != nil {
			return nil, fmt.Errorf("%s is corrupt: %w", path, err)
		}
	}
	if s.Cooldowns == nil {
		s.Cooldowns = map[string]int64{}
	}
	return s, nil
}

func (s *State) save(path string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".state-%d.tmp", os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// prune drops records that no longer affect any decision.
func (s *State) prune(now time.Time, cfg Config) {
	keep := time.Hour
	if cfg.DedupeWindow.Duration > keep {
		keep = cfg.DedupeWindow.Duration
	}
	cut := now.Add(-keep).UnixNano()
	writes := s.Writes[:0]
	for _, w := range s.Writes {
		if w.At > cut {
			writes = append(writes, w)
		}
	}
	s.Writes = writes

	strikeCut := now.Add(-cfg.BreakerWindow.Duration).UnixNano()
	strikes := s.Strikes[:0]
	for _, at := range s.Strikes {
		if at > strikeCut {
			strikes = append(strikes, at)
		}
	}
	s.Strikes = strikes

	for scope, until := range s.Cooldowns {
		if until <= now.UnixNano() {
			delete(s.Cooldowns, scope)
		}
	}
}

// gate returns why a request of the given kind may not start now, or nil.
func (s *State) gate(kind Kind, resource string, now time.Time) *Error {
	if s.Breaker != nil {
		return &Error{
			Kind:   ErrBreaker,
			Reason: "circuit breaker is open: " + s.Breaker.Reason,
		}
	}
	scopes := []string{scopeAll, resPrefix + resource}
	if kind == KindWrite {
		scopes = append(scopes, scopeWrite)
	}
	var until int64
	var which string
	for _, sc := range scopes {
		if u := s.Cooldowns[sc]; u > now.UnixNano() && u > until {
			until, which = u, sc
		}
	}
	if until == 0 {
		return nil
	}
	return &Error{
		Kind:    ErrCooldown,
		Reason:  fmt.Sprintf("cooling down after a GitHub rate limit (scope %q)", which),
		RetryAt: time.Unix(0, until),
		now:     now,
	}
}

func (s *State) isDuplicate(hash string, now time.Time, window time.Duration) bool {
	if hash == "" || window <= 0 {
		return false
	}
	cut := now.Add(-window).UnixNano()
	for _, w := range s.Writes {
		if w.Hash == hash && w.At > cut {
			return true
		}
	}
	return false
}

// writeWait reports how long the next write must wait to respect the
// interval and the rolling caps, and which limit forces the wait.
func (s *State) writeWait(now time.Time, cfg Config) (time.Duration, string) {
	var wait time.Duration
	var reason string
	consider := func(d time.Duration, why string) {
		if d > wait {
			wait, reason = d, why
		}
	}

	var last int64
	for _, w := range s.Writes {
		if w.At > last {
			last = w.At
		}
	}
	if last > 0 {
		consider(cfg.WriteInterval.Duration-now.Sub(time.Unix(0, last)),
			fmt.Sprintf("minimum interval of %s between writes", cfg.WriteInterval.Duration))
	}

	capWait := func(limit int, window time.Duration, label string) {
		if limit <= 0 {
			return
		}
		cut := now.Add(-window).UnixNano()
		var inWindow []int64
		for _, w := range s.Writes {
			if w.At > cut {
				inWindow = append(inWindow, w.At)
			}
		}
		if len(inWindow) < limit {
			return
		}
		// The write that must age out is the limit-th most recent one.
		slices.Sort(inWindow)
		oldest := inWindow[len(inWindow)-limit]
		consider(time.Unix(0, oldest).Add(window).Sub(now), fmt.Sprintf("cap of %d writes per %s", limit, label))
	}
	capWait(cfg.WritesPerMinute, time.Minute, "minute")
	capWait(cfg.WritesPerHour, time.Hour, "hour")
	return wait, reason
}

func (s *State) forget(hash string) {
	if hash == "" {
		return
	}
	for i := len(s.Writes) - 1; i >= 0; i-- {
		if s.Writes[i].Hash == hash {
			s.Writes[i].Hash = ""
			return
		}
	}
}

func (s *State) countWrites(now time.Time, window time.Duration) int {
	cut := now.Add(-window).UnixNano()
	n := 0
	for _, w := range s.Writes {
		if w.At > cut {
			n++
		}
	}
	return n
}
