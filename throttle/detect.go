package throttle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Rate-limit kinds.
const (
	limitPrimary   = "primary"   // hourly quota for a resource is used up
	limitSecondary = "secondary" // burst / abuse limit; GitHub may ban if ignored
	limitContent   = "content"   // content-creation limit; blocks writes only
)

type rateLimit struct {
	kind     string
	resource string
	retryAt  time.Time // zero when GitHub gave no hint
	message  string
}

// bufferResponse reads up to maxBuffer bytes of the response body and puts a
// complete body back on resp.
func bufferResponse(resp *http.Response) ([]byte, bool, error) {
	if resp.Body == nil || resp.Body == http.NoBody {
		return nil, true, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBuffer+1))
	if err != nil {
		_ = resp.Body.Close()
		return nil, false, err
	}
	if len(b) <= maxBuffer {
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(b))
		resp.ContentLength = int64(len(b))
		return b, true, nil
	}
	rest := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), rest), rest}
	return b[:maxBuffer], false, nil
}

var rateLimitPhrases = []string{
	"rate limit",
	"abuse detection",
	"submitted too quickly",
	"temporarily blocked",
}

func mentionsRateLimit(s string) bool {
	s = strings.ToLower(s)
	for _, p := range rateLimitPhrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

type apiErrorBody struct {
	Message string `json:"message"`
	Errors  []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"errors"`
}

// errorMessages extracts human-readable messages from a JSON error body, or
// a short prefix of a non-JSON body.
func errorMessages(body []byte) (string, []string) {
	var e apiErrorBody
	if json.Unmarshal(body, &e) != nil {
		s := strings.TrimSpace(string(body))
		if len(s) > 300 {
			s = s[:300]
		}
		return s, nil
	}
	msgs := []string{}
	types := []string{}
	if e.Message != "" {
		msgs = append(msgs, e.Message)
	}
	for _, x := range e.Errors {
		if x.Message != "" {
			msgs = append(msgs, x.Message)
		}
		types = append(types, x.Type)
	}
	return strings.Join(msgs, "; "), types
}

// detect inspects a response that GitHub returned and reports whether it is a
// rate limit. body is a buffered prefix of the response body.
func detect(resp *http.Response, body []byte, info *requestInfo, now time.Time) *rateLimit {
	h := resp.Header
	remainingZero := h.Get("X-Ratelimit-Remaining") == "0"
	msg, types := errorMessages(body)

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
	case resp.StatusCode == http.StatusForbidden:
		if !remainingZero && h.Get("Retry-After") == "" && !mentionsRateLimit(msg) {
			return nil // an ordinary permission error
		}
	case resp.StatusCode == http.StatusOK && info.graphql:
		limited := false
		for _, t := range types {
			if t == "RATE_LIMITED" || t == "RATE_LIMIT" {
				limited = true
			}
		}
		if !limited && !(len(types) > 0 && mentionsRateLimit(msg)) {
			return nil
		}
	default:
		return nil
	}

	rl := &rateLimit{message: msg, resource: h.Get("X-Ratelimit-Resource")}
	if rl.resource == "" {
		rl.resource = info.resource
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "content creation") || strings.Contains(lower, "submitted too quickly"):
		rl.kind = limitContent
	case strings.Contains(lower, "secondary") || strings.Contains(lower, "abuse"):
		rl.kind = limitSecondary
	case remainingZero || strings.Contains(lower, "api rate limit"):
		rl.kind = limitPrimary
	default:
		rl.kind = limitSecondary
	}

	if ra := h.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
			rl.retryAt = now.Add(time.Duration(secs) * time.Second)
		} else if t, err := http.ParseTime(ra); err == nil {
			rl.retryAt = t
		}
	}
	if rl.retryAt.IsZero() && remainingZero {
		if reset, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
			rl.retryAt = time.Unix(reset, 0).Add(time.Second)
		}
	}
	return rl
}

// apply records a rate limit in the shared state and returns the error to
// report for the request that hit it.
func (s *State) apply(rl *rateLimit, now time.Time, cfg Config) *Error {
	until := rl.retryAt
	scope := resPrefix + rl.resource

	if rl.kind == limitPrimary {
		// The quota refills at X-RateLimit-Reset; without it, wait a minute.
		if until.IsZero() {
			until = now.Add(time.Minute)
		}
	} else {
		s.Strikes = append(s.Strikes, now.UnixNano())
		n := 0
		cut := now.Add(-cfg.BreakerWindow.Duration).UnixNano()
		for _, at := range s.Strikes {
			if at > cut {
				n++
			}
		}
		delay := cfg.CooldownBase.Duration
		for i := 1; i < n && delay < cfg.CooldownMax.Duration; i++ {
			delay *= 2
		}
		if delay > cfg.CooldownMax.Duration {
			delay = cfg.CooldownMax.Duration
		}
		if backoff := now.Add(delay); backoff.After(until) {
			until = backoff
		}
		scope = scopeAll
		if rl.kind == limitContent {
			scope = scopeWrite
		}
		if n >= cfg.BreakerStrikes {
			s.Breaker = &breakerState{
				At:     now.UnixNano(),
				Reason: fmt.Sprintf("%d GitHub rate limits within %s; last: %s", n, cfg.BreakerWindow.Duration, rl.message),
			}
		}
	}
	if until.UnixNano() > s.Cooldowns[scope] {
		s.Cooldowns[scope] = until.UnixNano()
	}

	if s.Breaker != nil {
		return &Error{Kind: ErrBreaker, Reason: "circuit breaker opened: " + s.Breaker.Reason}
	}
	reason := "GitHub rate limited this account (" + rl.kind + ")"
	if rl.message != "" {
		reason += ": " + rl.message
	}
	return &Error{Kind: ErrRateLimited, Reason: reason, RetryAt: until, now: now}
}
