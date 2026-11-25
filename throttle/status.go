package throttle

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Cooldown is one active cooldown.
type Cooldown struct {
	Scope string
	Until time.Time
}

// Status is a snapshot of the shared throttle state for `gh throttle status`.
type Status struct {
	Dir           string
	ConfigPath    string
	Config        Config
	ConfigErr     error
	BreakerOpen   bool
	BreakerAt     time.Time
	BreakerReason string
	Cooldowns     []Cooldown
	Strikes       int
	WritesMinute  int
	WritesHour    int
	NextWrite     time.Duration // 0 when a write could start now
	NextWriteWhy  string
}

// Manager reads and resets shared throttle state outside of HTTP requests.
type Manager struct{ t *Transport }

// NewManager uses the same state directory and config as NewTransport.
func NewManager() *Manager { return &Manager{t: NewTransport(nil)} }

func (m *Manager) Dir() string { return m.t.dir }

// Status returns the current state.
func (m *Manager) Status(ctx context.Context) (*Status, error) {
	st := &Status{Dir: m.t.dir, ConfigPath: ConfigPath(), Config: m.t.cfg, ConfigErr: m.t.cfgErr}
	err := m.t.withState(ctx, func(s *State, now time.Time) {
		if s.Breaker != nil {
			st.BreakerOpen = true
			st.BreakerAt = time.Unix(0, s.Breaker.At)
			st.BreakerReason = s.Breaker.Reason
		}
		for scope, until := range s.Cooldowns {
			st.Cooldowns = append(st.Cooldowns, Cooldown{Scope: scope, Until: time.Unix(0, until)})
		}
		sort.Slice(st.Cooldowns, func(i, j int) bool { return st.Cooldowns[i].Scope < st.Cooldowns[j].Scope })
		st.Strikes = len(s.Strikes)
		st.WritesMinute = s.countWrites(now, time.Minute)
		st.WritesHour = s.countWrites(now, time.Hour)
		st.NextWrite, st.NextWriteWhy = s.writeWait(now, m.t.cfg)
	})
	return st, err
}

// Reset closes the breaker and clears cooldowns, strikes and the ghx read cache.
// Write history is kept so the rolling caps still apply. cacheDirs are extra
// directories to remove, such as gh's own HTTP cache, which may hold
// responses recorded while rate limited.
func (m *Manager) Reset(ctx context.Context, cacheDirs ...string) error {
	if err := m.t.withState(ctx, func(s *State, _ time.Time) {
		s.Breaker = nil
		s.Cooldowns = map[string]int64{}
		s.Strikes = nil
	}); err != nil {
		return err
	}
	for _, d := range append([]string{filepath.Join(m.t.dir, "ghx")}, cacheDirs...) {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
	}
	return nil
}

// LogFile is the path of the call log.
func (m *Manager) LogFile() string { return LogPath(m.t.dir) }
