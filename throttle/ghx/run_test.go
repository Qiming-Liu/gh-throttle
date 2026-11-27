package ghx

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeGH struct {
	mu    sync.Mutex
	calls [][]string
	out   func(args []string) *Result
}

func (f *fakeGH) exec(args []string) *Result {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.mu.Unlock()
	return f.out(args)
}

func (f *fakeGH) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newTestRunner(t *testing.T, out func([]string) *Result) (*Runner, *fakeGH, *time.Time) {
	t.Helper()
	gh := &fakeGH{out: out}
	r := NewRunner(Options{
		Dir:             t.TempDir(),
		TTL:             30 * time.Second,
		TTLOverrides:    map[string]time.Duration{},
		ImmutableTTL:    time.Hour,
		SettledTTL:      10 * time.Minute,
		MaxCacheEntries: 1000,
		MaxCacheBytes:   64 << 20,
		LockTimeout:     time.Minute,
	}, gh.exec)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	r.store.now = func() time.Time { return now }
	return r, gh, &now
}

var testCtx = ExecContext{Host: "github.com", Repo: "o/r", Branch: "main", TokenHash: "t"}

func run(r *Runner, args ...string) *Result {
	return r.Cached(args, testCtx, r.Classify(args), false, 0)
}

func TestCachedHitWithinTTL(t *testing.T) {
	r, gh, now := newTestRunner(t, func([]string) *Result { return &Result{Stdout: []byte("1\n")} })
	run(r, "pr", "list")
	res := run(r, "pr", "list")
	if string(res.Stdout) != "1\n" || gh.count() != 1 {
		t.Fatalf("want one call and cached output, got %d calls, %q", gh.count(), res.Stdout)
	}
	*now = now.Add(31 * time.Second)
	run(r, "pr", "list")
	if gh.count() != 2 {
		t.Fatalf("expired entry should refetch, calls=%d", gh.count())
	}
}

func TestJQReappliedToSharedResponse(t *testing.T) {
	r, gh, _ := newTestRunner(t, func(args []string) *Result {
		for _, a := range args {
			if a == "--jq" {
				t.Fatalf("--jq must be stripped from the fetch: %v", args)
			}
		}
		return &Result{Stdout: []byte(`[{"number":1,"title":"a"}]` + "\n")}
	})
	a := run(r, "pr", "list", "--json", "title,number", "--jq", ".[0].number")
	b := run(r, "pr", "list", "--json", "number,title", "--jq", ".[0].title")
	if string(a.Stdout) != "1\n" || string(b.Stdout) != "a\n" {
		t.Fatalf("got %q and %q", a.Stdout, b.Stdout)
	}
	if gh.count() != 1 {
		t.Fatalf("field order and jq must share one fetch, calls=%d", gh.count())
	}
}

func TestSupersetFieldsProjected(t *testing.T) {
	r, gh, _ := newTestRunner(t, func([]string) *Result {
		return &Result{Stdout: []byte(`[{"number":1,"title":"a"}]` + "\n")}
	})
	run(r, "pr", "list", "--json", "number,title")
	res := run(r, "pr", "list", "--json", "number")
	if string(res.Stdout) != `[{"number":1}]`+"\n" || gh.count() != 1 {
		t.Fatalf("got %q with %d calls", res.Stdout, gh.count())
	}
}

func TestMutationInvalidatesNamespace(t *testing.T) {
	r, gh, _ := newTestRunner(t, func([]string) *Result { return &Result{Stdout: []byte("x")} })
	run(r, "pr", "list")
	run(r, "issue", "list")
	r.Invalidate(r.Classify([]string{"pr", "comment", "1", "-b", "hi"}), testCtx)
	run(r, "pr", "list")
	run(r, "issue", "list")
	if gh.count() != 3 {
		t.Fatalf("pr list refetched, issue list still cached: want 3 calls, got %d", gh.count())
	}
}

func TestAPIWriteInvalidatesRepo(t *testing.T) {
	r, gh, _ := newTestRunner(t, func([]string) *Result { return &Result{Stdout: []byte("x")} })
	run(r, "pr", "list")
	run(r, "issue", "list")
	cl := r.Classify([]string{"api", "-X", "POST", "repos/o/r/issues", "-f", "title=x"})
	if cl.Type != Mutation {
		t.Fatalf("want mutation, got %v", cl.Type)
	}
	r.Invalidate(cl, testCtx)
	run(r, "pr", "list")
	run(r, "issue", "list")
	if gh.count() != 4 {
		t.Fatalf("api write flushes the whole repo: want 4 calls, got %d", gh.count())
	}
}

func TestStorableFilter(t *testing.T) {
	r, gh, _ := newTestRunner(t, func([]string) *Result { return &Result{ExitCode: 75, Stderr: []byte("gh-throttle")} })
	r.opts.Storable = func(res *Result) bool { return res.ExitCode != 75 }
	run(r, "pr", "list")
	run(r, "pr", "list")
	if gh.count() != 2 {
		t.Fatalf("refusals must not be cached, calls=%d", gh.count())
	}
}

func TestImmutableTTL(t *testing.T) {
	r, gh, now := newTestRunner(t, func([]string) *Result { return &Result{Stdout: []byte("{}")} })
	sha := strings.Repeat("a", 40)
	run(r, "api", "repos/o/r/commits/"+sha)
	*now = now.Add(59 * time.Minute)
	run(r, "api", "repos/o/r/commits/"+sha)
	if gh.count() != 1 {
		t.Fatalf("SHA-addressed content keeps for immutable_ttl, calls=%d", gh.count())
	}
}

func TestSettledTTL(t *testing.T) {
	r, gh, now := newTestRunner(t, func([]string) *Result { return &Result{Stdout: []byte(`{"status":"completed"}`)} })
	run(r, "run", "view", "123", "--json", "status")
	*now = now.Add(9 * time.Minute)
	run(r, "run", "view", "123", "--json", "status")
	if gh.count() != 1 {
		t.Fatalf("completed runs keep for settled_ttl, calls=%d", gh.count())
	}
}

func TestNoCacheStillStores(t *testing.T) {
	r, gh, _ := newTestRunner(t, func([]string) *Result { return &Result{Stdout: []byte("x")} })
	args := []string{"pr", "list"}
	r.Cached(args, testCtx, r.Classify(args), true, 0)
	r.Cached(args, testCtx, r.Classify(args), true, 0)
	run(r, "pr", "list")
	if gh.count() != 2 {
		t.Fatalf("--no-cache fetches but stores for others, calls=%d", gh.count())
	}
}

func TestConcurrentIdenticalCallsCoalesce(t *testing.T) {
	var gate sync.WaitGroup
	gate.Add(1)
	r, gh, _ := newTestRunner(t, func([]string) *Result {
		gate.Wait()
		return &Result{Stdout: []byte("x")}
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); run(r, "pr", "list") }()
	}
	time.Sleep(200 * time.Millisecond)
	gate.Done()
	wg.Wait()
	if gh.count() != 1 {
		t.Fatalf("identical in-flight calls share one execution, calls=%d", gh.count())
	}
}

func TestLRUEviction(t *testing.T) {
	r, _, _ := newTestRunner(t, func(args []string) *Result { return &Result{Stdout: []byte(args[len(args)-1])} })
	r.store.maxEntries = 2
	run(r, "issue", "view", "1")
	time.Sleep(20 * time.Millisecond)
	run(r, "issue", "view", "2")
	time.Sleep(20 * time.Millisecond)
	run(r, "issue", "view", "3")
	n := 0
	for _, a := range []string{"1", "2", "3"} {
		args := []string{"issue", "view", a}
		cl := r.Classify(args)
		if r.store.Contains(CacheKey(keyContext(testCtx, cl), Normalize(args).Fetch)) {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want 2 entries after eviction, got %d", n)
	}
}
