package ghx

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cli/cli/v2/throttle/internal/flock"
)

// Result is the captured output of one gh run.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Options are ghx's cache settings.
type Options struct {
	Dir             string
	TTL             time.Duration
	TTLOverrides    map[string]time.Duration
	ImmutableTTL    time.Duration
	SettledTTL      time.Duration
	MaxCacheEntries int
	MaxCacheBytes   int64
	Additional      []string
	// LockTimeout bounds how long a caller waits for an identical in-flight
	// command (singleflight) before running its own.
	LockTimeout time.Duration
	// Storable reports whether a result may be cached. ghx caches every
	// result; gh-throttle uses this to never replay its own refusals.
	Storable func(*Result) bool
}

// Runner applies ghx's caching to gh commands. Exec runs gh with the given
// arguments in a child process and captures its output, as ghx's daemon does.
type Runner struct {
	opts       Options
	store      *Store
	classifier *Classifier
	Exec       func(args []string) *Result
}

func NewRunner(opts Options, exec func([]string) *Result) *Runner {
	return &Runner{
		opts:       opts,
		store:      NewStore(opts.Dir, opts.MaxCacheEntries, opts.MaxCacheBytes),
		classifier: NewClassifier(opts.Additional),
		Exec:       exec,
	}
}

func (r *Runner) Store() *Store { return r.store }

// Classify is ghx's allowlist classification.
func (r *Runner) Classify(args []string) Classification { return r.classifier.Classify(args) }

func (r *Runner) commandTTL(cmdKey string) time.Duration {
	if ttl, ok := r.opts.TTLOverrides[cmdKey]; ok {
		return ttl
	}
	return r.opts.TTL
}

// Cached runs a Cacheable command through the cache, following ghx's
// Handler.handleExec: check the cache, then any entry that fetched a superset
// of the --json fields, then coalesce with an identical in-flight command,
// then execute and store. Output filters are re-applied to the shared result.
func (r *Runner) Cached(args []string, ctx ExecContext, cl Classification, noCache bool, ttlOverride time.Duration) *Result {
	plan := Normalize(args)
	kctx := keyContext(ctx, cl)
	cacheKey := CacheKey(kctx, plan.Fetch)
	var baseKey string
	if plan.Base != nil {
		baseKey = CacheKey(kctx, plan.Base)
	}

	// --no-cache skips the lookup but still stores the fresh result so other
	// callers reuse it.
	if noCache {
		res := r.Exec(plan.Fetch)
		r.store1(cacheKey, cl, ctx, ttlOverride, res)
		r.store.IndexFields(baseKey, cacheKey, plan.Fields)
		return r.finish(plan, args, res)
	}

	if e := r.store.Get(cacheKey); e != nil {
		return r.finish(plan, args, entryResult(e))
	}
	if e := r.store.LookupSuperset(baseKey, plan.Fields); e != nil {
		return r.finish(plan, args, entryResult(e))
	}

	// Singleflight across processes: one caller runs the command while
	// identical callers wait on the key's lock, then read its result.
	if path, err := r.store.LockPath(cacheKey); err == nil {
		if lk, err := flock.Acquire(context.Background(), path, r.opts.LockTimeout); err == nil {
			defer lk.Release()
			if e := r.store.Get(cacheKey); e != nil {
				return r.finish(plan, args, entryResult(e)) // coalesced
			}
		}
	}

	res := r.Exec(plan.Fetch)
	r.store1(cacheKey, cl, ctx, ttlOverride, res)
	r.store.IndexFields(baseKey, cacheKey, plan.Fields)
	return r.finish(plan, args, res)
}

func entryResult(e *Entry) *Result {
	return &Result{Stdout: e.Stdout, Stderr: e.Stderr, ExitCode: e.ExitCode}
}

var errNonZeroExit = errors.New("non-zero exit")

// finish applies the caller's --jq filter to a fetched result. gh reports
// failures before filtering, so on any error the original command runs
// directly to reproduce gh's exact output.
func (r *Runner) finish(plan Plan, args []string, res *Result) *Result {
	if plan.JQ == "" {
		return res
	}
	var out []byte
	err := errNonZeroExit
	if res.ExitCode == 0 {
		out, err = ApplyJQ(res.Stdout, plan.JQ)
	}
	if err != nil {
		return r.Exec(args)
	}
	return &Result{Stdout: out, Stderr: res.Stderr, ExitCode: 0}
}

func (r *Runner) store1(cacheKey string, cl Classification, ctx ExecContext, ttlOverride time.Duration, res *Result) {
	if r.opts.Storable != nil && !r.opts.Storable(res) {
		return
	}
	ttl := r.commandTTL(cl.CmdKey)
	if ttlOverride > 0 {
		ttl = ttlOverride
	} else if _, overridden := r.opts.TTLOverrides[cl.CmdKey]; !overridden && res.ExitCode == 0 {
		if cl.Immutable && r.opts.ImmutableTTL > ttl {
			ttl = r.opts.ImmutableTTL
		} else if cl.Settles && r.opts.SettledTTL > ttl && reportsCompleted(res.Stdout) {
			ttl = r.opts.SettledTTL
		}
	}
	host, repo := cl.Target(ctx.Host, ctx.Repo)
	r.store.Set(&Entry{
		Key:      cacheKey,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
		CachedAt: r.store.now(),
		TTL:      ttl,
		Resource: cl.Resource,
		Host:     host,
		Repo:     repo,
	})
}

// Invalidate drops entries affected by a mutation, as ghx does after a
// mutating command finishes, including failures that may have partially
// changed remote state. Raw API reads can observe any resource, so they are
// flushed alongside it; a raw API write can change any resource, so it
// flushes everything cached for the repository.
func (r *Runner) Invalidate(cl Classification, ctx ExecContext) int {
	if cl.Resource == ResourceUnknown {
		return 0
	}
	host, repo := cl.Target(ctx.Host, ctx.Repo)
	if cl.Resource == ResourceAPI {
		return r.store.InvalidateRepo(host, repo)
	}
	return r.store.InvalidateNamespace(host, repo, cl.Resource) +
		r.store.InvalidateNamespace(host, repo, ResourceAPI)
}

// reportsCompleted reports whether stdout is a JSON object whose top-level
// "status" is "completed" (the terminal state for Actions runs and jobs).
func reportsCompleted(stdout []byte) bool {
	var v struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(stdout, &v) == nil && v.Status == "completed"
}

// keyContext drops context the command's output cannot depend on, so callers
// in different checkouts or branches share entries.
func keyContext(ctx ExecContext, cl Classification) ExecContext {
	if cl.IgnoreBranch {
		ctx.Branch = ""
	}
	if cl.IgnoreRepo {
		ctx.Repo = ""
	}
	if cl.TargetHost != "" {
		ctx.Host = cl.TargetHost
	}
	if cl.TargetRepo != "" {
		ctx.Repo = cl.TargetRepo
	}
	return ctx
}
