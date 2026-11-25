package throttle

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGitHub answers requests with a canned response and counts calls.
type fakeGitHub struct {
	mu      sync.Mutex
	calls   int
	respond func(*http.Request) *http.Response
}

func (f *fakeGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if req.Body != nil {
		_, _ = io.ReadAll(req.Body)
	}
	return f.respond(req), nil
}

func (f *fakeGitHub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func reply(status int, body string, headers ...string) *http.Response {
	h := http.Header{"Content-Type": {"application/json"}}
	for i := 0; i+1 < len(headers); i += 2 {
		h.Set(headers[i], headers[i+1])
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) sleep(_ context.Context, d time.Duration) error {
	c.t = c.t.Add(d)
	return nil
}

func setup(t *testing.T, respond func(*http.Request) *http.Response, mod func(*Config)) (*Transport, *fakeGitHub, *clock) {
	t.Helper()
	cfg := DefaultConfig()
	if mod != nil {
		mod(&cfg)
	}
	gh := &fakeGitHub{respond: respond}
	tr := newTransport(gh, t.TempDir(), cfg, nil)
	c := &clock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	tr.now = c.now
	tr.sleep = c.sleep
	return tr, gh, c
}

func ok(*http.Request) *http.Response { return reply(200, `{"ok":true}`) }

func do(t *testing.T, tr *Transport, method, url, body string) (*http.Response, error) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	require.NoError(t, err)
	req.Header.Set("Authorization", "token abc")
	resp, err := tr.RoundTrip(req)
	if resp != nil {
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	return resp, err
}

func throttleErr(t *testing.T, err error) *Error {
	t.Helper()
	var te *Error
	require.True(t, errors.As(err, &te), "expected *throttle.Error, got %v", err)
	return te
}

const issues = "https://api.github.com/repos/o/r/issues"

func TestClassify(t *testing.T) {
	cases := []struct {
		method, url, body string
		want              Kind
	}{
		{"GET", issues, "", KindRead},
		{"POST", issues, `{"title":"x"}`, KindWrite},
		{"PATCH", issues + "/1", `{}`, KindWrite},
		{"DELETE", issues + "/1", "", KindWrite},
		{"POST", "https://api.github.com/graphql", `{"query":"query { viewer { login } }"}`, KindRead},
		{"POST", "https://api.github.com/graphql", `{"query":"{ viewer { login } }"}`, KindRead},
		{"POST", "https://api.github.com/graphql", `{"query":"mutation($i:ID!){ closeIssue(input:{issueId:$i}){clientMutationId} }"}`, KindWrite},
		{"POST", "https://api.github.com/graphql", `{"query":"# mutation in a comment\nquery { viewer { login } }"}`, KindRead},
		{"POST", "https://api.github.com/graphql", `not json`, KindWrite},
		{"POST", "https://ghe.example.com/api/graphql", `{"query":"mutation { x }"}`, KindWrite},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.url+" "+c.body, func(t *testing.T) {
			req, _ := http.NewRequest(c.method, c.url, strings.NewReader(c.body))
			info, err := inspect(req)
			require.NoError(t, err)
			assert.Equal(t, c.want, info.kind)
			b, _ := io.ReadAll(req.Body)
			assert.Equal(t, c.body, string(b), "body must be replayable")
		})
	}
}

func TestGoverned(t *testing.T) {
	for url, want := range map[string]bool{
		"https://api.github.com/user":                    true,
		"https://uploads.github.com/repos/o/r/releases":  true,
		"https://ghe.example.com/api/v3/user":            true,
		"https://github.com/login/oauth/access_token":    false,
		"http://127.0.0.1:8080/api/v3/user":              false,
		"http://localhost/api/graphql":                   false,
		"https://objects.githubusercontent.com/whatever": false,
	} {
		req, _ := http.NewRequest("GET", url, nil)
		assert.Equal(t, want, governed(req), url)
	}
}

func TestWriteInterval(t *testing.T) {
	tr, gh, c := setup(t, ok, nil)
	start := c.t
	_, err := do(t, tr, "POST", issues, `{"title":"a"}`)
	require.NoError(t, err)
	_, err = do(t, tr, "POST", issues, `{"title":"b"}`)
	require.NoError(t, err)
	assert.Equal(t, 2, gh.count())
	assert.Equal(t, 3*time.Second, c.t.Sub(start), "second write waits for the interval")
}

func TestDuplicateWriteRefused(t *testing.T) {
	tr, gh, c := setup(t, ok, nil)
	_, err := do(t, tr, "POST", issues, `{"title":"same"}`)
	require.NoError(t, err)
	_, err = do(t, tr, "POST", issues, `{"title":"same"}`)
	assert.Equal(t, ErrDuplicate, throttleErr(t, err).Kind)
	assert.Equal(t, 1, gh.count())

	c.t = c.t.Add(61 * time.Second)
	_, err = do(t, tr, "POST", issues, `{"title":"same"}`)
	require.NoError(t, err, "allowed again after the dedupe window")
}

func TestFailedWriteMayBeRetried(t *testing.T) {
	status := 422
	tr, gh, _ := setup(t, func(*http.Request) *http.Response { return reply(status, `{"message":"Validation Failed"}`) }, nil)
	resp, err := do(t, tr, "POST", issues, `{"title":"x"}`)
	require.NoError(t, err)
	assert.Equal(t, 422, resp.StatusCode)
	status = 201
	_, err = do(t, tr, "POST", issues, `{"title":"x"}`)
	require.NoError(t, err)
	assert.Equal(t, 2, gh.count())
}

func TestPerMinuteCapRefusesBeyondMaxWait(t *testing.T) {
	tr, gh, _ := setup(t, ok, func(c *Config) {
		c.WriteInterval = Duration{0}
		c.WritesPerMinute = 2
		c.MaxWait = Duration{10 * time.Second}
	})
	for i := 0; i < 2; i++ {
		_, err := do(t, tr, "POST", issues, `{"n":`+string(rune('0'+i))+`}`)
		require.NoError(t, err)
	}
	_, err := do(t, tr, "POST", issues, `{"n":9}`)
	te := throttleErr(t, err)
	assert.Equal(t, ErrLimit, te.Kind)
	assert.Contains(t, te.Reason, "2 writes per minute")
	assert.Equal(t, 2, gh.count())
}

func TestPerHourCap(t *testing.T) {
	tr, _, c := setup(t, ok, func(c *Config) {
		c.WriteInterval = Duration{0}
		c.WritesPerMinute = 0
		c.WritesPerHour = 3
	})
	for i := 0; i < 3; i++ {
		_, err := do(t, tr, "POST", issues, `{"n":`+string(rune('0'+i))+`}`)
		require.NoError(t, err)
		c.t = c.t.Add(time.Minute)
	}
	_, err := do(t, tr, "POST", issues, `{"n":9}`)
	assert.Equal(t, ErrLimit, throttleErr(t, err).Kind)
}

func TestSecondaryRateLimitCoolsDownEverything(t *testing.T) {
	limited := true
	tr, gh, c := setup(t, func(*http.Request) *http.Response {
		if limited {
			return reply(403, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)
		}
		return reply(200, `{}`)
	}, nil)

	_, err := do(t, tr, "POST", issues, `{"title":"x"}`)
	te := throttleErr(t, err)
	assert.Equal(t, ErrRateLimited, te.Kind)
	assert.Equal(t, c.t.Add(time.Minute), te.RetryAt)

	limited = false
	_, err = do(t, tr, "GET", "https://api.github.com/user", "")
	assert.Equal(t, ErrCooldown, throttleErr(t, err).Kind, "reads are blocked too")
	assert.Equal(t, 1, gh.count())

	c.t = c.t.Add(61 * time.Second)
	_, err = do(t, tr, "GET", "https://api.github.com/user", "")
	require.NoError(t, err)
}

func TestRetryAfterIsHonored(t *testing.T) {
	tr, _, c := setup(t, func(*http.Request) *http.Response {
		return reply(429, `{"message":"secondary rate limit"}`, "Retry-After", "600")
	}, nil)
	_, err := do(t, tr, "GET", issues, "")
	assert.Equal(t, c.t.Add(10*time.Minute), throttleErr(t, err).RetryAt)
}

func TestHTMLTooManyRequests(t *testing.T) {
	tr, _, _ := setup(t, func(*http.Request) *http.Response {
		r := reply(429, `<html>Whoa there! You have exceeded a secondary rate limit.</html>`)
		r.Header.Set("Content-Type", "text/html")
		return r
	}, nil)
	_, err := do(t, tr, "POST", issues, `{"x":1}`)
	assert.Equal(t, ErrRateLimited, throttleErr(t, err).Kind)
}

func TestContentCreationLimitBlocksOnlyWrites(t *testing.T) {
	limited := true
	tr, _, _ := setup(t, func(req *http.Request) *http.Response {
		if limited && req.Method == "POST" {
			return reply(403, `{"message":"You have exceeded a secondary rate limit and have been temporarily blocked from content creation."}`)
		}
		return reply(200, `{}`)
	}, nil)
	_, err := do(t, tr, "POST", issues, `{"x":1}`)
	assert.Equal(t, ErrRateLimited, throttleErr(t, err).Kind)
	limited = false
	_, err = do(t, tr, "GET", issues, "")
	require.NoError(t, err, "reads continue during a content-creation cooldown")
	_, err = do(t, tr, "POST", issues, `{"x":2}`)
	assert.Equal(t, ErrCooldown, throttleErr(t, err).Kind)
}

func TestPrimaryLimitCoolsDownOnlyThatResource(t *testing.T) {
	tr, _, c := setup(t, func(req *http.Request) *http.Response {
		if strings.Contains(req.URL.Path, "/search/") {
			return reply(403, `{"message":"API rate limit exceeded for user ID 1."}`,
				"X-Ratelimit-Remaining", "0", "X-Ratelimit-Resource", "search",
				"X-Ratelimit-Reset", "1795608030")
		}
		return reply(200, `{}`)
	}, nil)
	_, err := do(t, tr, "GET", "https://api.github.com/search/issues?q=x", "")
	te := throttleErr(t, err)
	assert.Equal(t, time.Unix(1795608031, 0), te.RetryAt)
	_, err = do(t, tr, "GET", issues, "")
	require.NoError(t, err, "core is unaffected")
	st, _ := (&Manager{t: tr}).Status(context.Background())
	assert.Equal(t, 0, st.Strikes, "primary limits are not strikes")
	_ = c
}

func TestGraphQLRateLimitIn200(t *testing.T) {
	tr, _, _ := setup(t, func(*http.Request) *http.Response {
		return reply(200, `{"errors":[{"type":"RATE_LIMIT","message":"API rate limit already exceeded for user ID 1."}]}`)
	}, nil)
	_, err := do(t, tr, "POST", "https://api.github.com/graphql", `{"query":"{ viewer { login } }"}`)
	assert.Equal(t, ErrRateLimited, throttleErr(t, err).Kind)
}

func TestPlainForbiddenIsNotARateLimit(t *testing.T) {
	tr, _, _ := setup(t, func(*http.Request) *http.Response {
		return reply(403, `{"message":"Resource not accessible by integration"}`)
	}, nil)
	resp, err := do(t, tr, "POST", issues, `{"x":1}`)
	require.NoError(t, err)
	assert.Equal(t, 403, resp.StatusCode)
}

func TestBreakerOpensAfterThreeStrikes(t *testing.T) {
	tr, gh, c := setup(t, func(*http.Request) *http.Response {
		return reply(429, `{"message":"secondary rate limit"}`)
	}, nil)
	var err error
	for i := 0; i < 3; i++ {
		_, err = do(t, tr, "GET", issues, "")
		require.Error(t, err)
		c.t = c.t.Add(35 * time.Minute / 2) // wait out each cooldown, stay inside the hour
		_ = i
	}
	assert.Equal(t, ErrBreaker, throttleErr(t, err).Kind)

	c.t = c.t.Add(3 * time.Hour)
	_, err = do(t, tr, "GET", issues, "")
	assert.Equal(t, ErrBreaker, throttleErr(t, err).Kind, "breaker stays open until reset")
	assert.Equal(t, 3, gh.count())

	m := &Manager{t: tr}
	require.NoError(t, m.Reset(context.Background()))
	_, err = do(t, tr, "GET", issues, "")
	assert.Equal(t, ErrRateLimited, throttleErr(t, err).Kind, "calls flow again after reset")
}

func TestCooldownDoublesPerStrike(t *testing.T) {
	tr, _, c := setup(t, func(*http.Request) *http.Response {
		return reply(429, `{"message":"secondary rate limit"}`)
	}, func(cfg *Config) { cfg.BreakerStrikes = 10 })
	var waits []time.Duration
	for i := 0; i < 3; i++ {
		_, err := do(t, tr, "GET", issues, "")
		te := throttleErr(t, err)
		waits = append(waits, te.RetryAt.Sub(c.t))
		c.t = te.RetryAt
	}
	assert.Equal(t, []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}, waits)
}

func TestUngovernedPassesThrough(t *testing.T) {
	tr, gh, _ := setup(t, func(*http.Request) *http.Response { return reply(429, `rate limit`) }, nil)
	for i := 0; i < 5; i++ {
		resp, err := do(t, tr, "POST", "https://github.com/login/oauth/access_token", `same`)
		require.NoError(t, err)
		assert.Equal(t, 429, resp.StatusCode)
	}
	assert.Equal(t, 5, gh.count())
}

func TestConcurrentWritesAreSerialized(t *testing.T) {
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	gh := &fakeGitHub{respond: func(*http.Request) *http.Response {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return reply(201, `{}`)
	}}
	cfg := DefaultConfig()
	cfg.WriteInterval = Duration{0}
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A separate Transport per goroutine stands in for separate gh processes.
			tr := newTransport(gh, dir, cfg, nil)
			_, err := do(t, tr, "POST", issues, `{"n":`+string(rune('0'+i))+`}`)
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()
	assert.Equal(t, 1, maxInFlight)
	assert.Equal(t, 4, gh.count())
}

func TestInvalidConfigFailsClosed(t *testing.T) {
	gh := &fakeGitHub{respond: ok}
	tr := newTransport(gh, t.TempDir(), DefaultConfig(), errors.New("bad yaml"))
	_, err := do(t, tr, "GET", issues, "")
	assert.Equal(t, ErrState, throttleErr(t, err).Kind)
	assert.Equal(t, 0, gh.count())
}

func TestErrorMessage(t *testing.T) {
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local)
	e := &Error{Kind: ErrCooldown, Reason: "cooling down", RetryAt: now.Add(90 * time.Second), now: now}
	assert.Equal(t, "gh-throttle: cooling down\nDo NOT retry before 10:01:30 (in 1m30s). Continue with other work or tell the user.", e.Error())
}
