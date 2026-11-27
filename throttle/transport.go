package throttle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cli/cli/v2/throttle/internal/flock"
)

// Transport is the throttling http.RoundTripper.
type Transport struct {
	next     http.RoundTripper
	dir      string
	cfg      Config
	cfgErr   error
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	lockWait time.Duration // timeout for the short state lock
}

// tripped records that this process saw a throttle error, so Main can exit
// with ExitCode even though gh printed the error itself.
var tripped atomic.Bool

// NewTransport wraps next with throttling, using the shared state directory
// and the user's throttle.yml.
func NewTransport(next http.RoundTripper) *Transport {
	cfg, err := LoadConfig(ConfigPath())
	return newTransport(next, Dir(), cfg, err)
}

func newTransport(next http.RoundTripper, dir string, cfg Config, cfgErr error) *Transport {
	if next == nil {
		next = http.DefaultTransport
	}
	return &Transport{
		next:     next,
		dir:      dir,
		cfg:      cfg,
		cfgErr:   cfgErr,
		now:      time.Now,
		sleep:    sleepCtx,
		lockWait: 30 * time.Second,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (t *Transport) statePath() string { return filepath.Join(t.dir, "state.json") }

// withState runs fn on the shared state while holding the state lock, then
// saves it.
func (t *Transport) withState(ctx context.Context, fn func(*State, time.Time)) error {
	if err := os.MkdirAll(t.dir, 0o700); err != nil {
		return err
	}
	l, err := flock.Acquire(ctx, filepath.Join(t.dir, "state.lock"), t.lockWait)
	if err != nil {
		return err
	}
	defer l.Release()
	s, err := loadState(t.statePath())
	if err != nil {
		return err
	}
	now := t.now()
	s.prune(now, t.cfg)
	fn(s, now)
	return s.save(t.statePath())
}

func stateError(err error) *Error {
	return &Error{Kind: ErrState, Reason: "cannot use shared throttle state: " + err.Error()}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !governed(req) {
		return t.next.RoundTrip(req)
	}
	resp, err := t.roundTrip(req)
	if _, ok := errors.AsType[*Error](err); ok {
		tripped.Store(true)
	}
	return resp, err
}

func (t *Transport) roundTrip(req *http.Request) (*http.Response, error) {
	if t.cfgErr != nil {
		return nil, &Error{Kind: ErrState, Reason: "invalid throttle config: " + t.cfgErr.Error()}
	}
	start := t.now()
	info, err := inspect(req)
	if err != nil {
		return nil, err
	}
	entry := logEntry{
		Time:   start,
		PID:    os.Getpid(),
		Kind:   info.kind,
		Method: req.Method,
		Host:   req.URL.Host,
		Path:   req.URL.Path,
		Agent:  agentFrom(req.Header.Get("User-Agent")),
	}
	if info.kind == KindWrite && len(info.hash) >= 12 {
		entry.Hash = info.hash[:12]
	}
	var resp *http.Response
	if info.kind == KindWrite {
		resp, err = t.write(req, info, &entry)
	} else {
		resp, err = t.read(req, info, &entry)
	}
	entry.Duration = t.now().Sub(start).Milliseconds()
	if err != nil && entry.Outcome == "" {
		entry.Outcome = "error"
		entry.Reason = err.Error()
	}
	appendLog(t.dir, entry)
	return resp, err
}

func (t *Transport) read(req *http.Request, info *requestInfo, entry *logEntry) (*http.Response, error) {
	var blocked *Error
	if err := t.withState(req.Context(), func(s *State, now time.Time) {
		blocked = s.gate(KindRead, info.resource, now)
	}); err != nil {
		return nil, stateError(err)
	}
	if blocked != nil {
		entry.Outcome, entry.Reason = "blocked", blocked.Reason
		return nil, blocked
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	return t.inspectResponse(req, info, resp, entry)
}

func (t *Transport) write(req *http.Request, info *requestInfo, entry *logEntry) (*http.Response, error) {
	ctx := req.Context()
	if err := os.MkdirAll(t.dir, 0o700); err != nil {
		return nil, stateError(err)
	}
	// The write lock queues writes from every gh process; it is held until
	// the response arrives, so at most one write is in flight at a time.
	wl, err := flock.Acquire(ctx, filepath.Join(t.dir, "write.lock"), t.cfg.LockTimeout.Duration)
	if err != nil {
		return nil, &Error{Kind: ErrLimit, Reason: "waiting for queued writes: " + err.Error(), RetryAt: t.now().Add(time.Minute), now: t.now()}
	}
	defer wl.Release()

	queued := t.now()
	for {
		var blocked *Error
		var wait time.Duration
		var why string
		if err := t.withState(ctx, func(s *State, now time.Time) {
			if blocked = s.gate(KindWrite, info.resource, now); blocked != nil {
				return
			}
			if s.isDuplicate(info.hash, now, t.cfg.DedupeWindow.Duration) {
				blocked = &Error{
					Kind:   ErrDuplicate,
					Reason: fmt.Sprintf("refused duplicate write: an identical %s %s succeeded within the last %s", req.Method, req.URL.Path, t.cfg.DedupeWindow.Duration),
				}
				return
			}
			if wait, why = s.writeWait(now, t.cfg); wait > 0 {
				return
			}
			s.Writes = append(s.Writes, writeRecord{At: now.UnixNano(), Hash: info.hash})
		}); err != nil {
			return nil, stateError(err)
		}
		if blocked != nil {
			entry.Outcome, entry.Reason = "blocked", blocked.Reason
			return nil, blocked
		}
		if wait <= 0 {
			break
		}
		now := t.now()
		if now.Add(wait).Sub(queued) > t.cfg.MaxWait.Duration {
			e := &Error{Kind: ErrLimit, Reason: "write limit reached: " + why, RetryAt: now.Add(wait), now: now}
			entry.Outcome, entry.Reason = "blocked", e.Reason
			return nil, e
		}
		if err := t.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	// Include time spent queued behind other processes' writes.
	entry.Waited = t.now().Sub(entry.Time).Milliseconds()

	resp, err := t.next.RoundTrip(req)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A failed write may be retried right away; only success counts for dedupe.
		_ = t.withState(context.WithoutCancel(ctx), func(s *State, _ time.Time) { s.forget(info.hash) })
	}
	if err != nil {
		return nil, err
	}
	return t.inspectResponse(req, info, resp, entry)
}

// inspectResponse turns rate-limit responses into errors, so nothing above it
// (gh's own HTTP cache, the ghx read cache, or the command) treats them as data.
func (t *Transport) inspectResponse(req *http.Request, info *requestInfo, resp *http.Response, entry *logEntry) (*http.Response, error) {
	entry.Status = resp.StatusCode
	needBody := resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests ||
		(info.graphql && resp.StatusCode == http.StatusOK)
	if !needBody {
		entry.Outcome = "sent"
		return resp, nil
	}
	body, _, err := bufferResponse(resp)
	if err != nil {
		return nil, err
	}
	if rl := detect(resp, body, info, t.now()); rl != nil {
		_ = resp.Body.Close()
		var rlErr *Error
		if err := t.withState(context.WithoutCancel(req.Context()), func(s *State, now time.Time) {
			rlErr = s.apply(rl, now, t.cfg)
		}); err != nil {
			return nil, stateError(err)
		}
		entry.Outcome, entry.Reason = "ratelimited", rlErr.Reason
		return nil, rlErr
	}
	entry.Outcome = "sent"
	return resp, nil
}

func agentFrom(ua string) string {
	_, after, ok := strings.Cut(ua, "Agent/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(after, " ")
	return name
}
