package throttle

import (
	"fmt"
	"strings"
	"time"
)

// ErrorKind says why gh-throttle refused or failed a request.
type ErrorKind string

const (
	ErrBreaker     ErrorKind = "breaker"     // circuit breaker open; needs `gh throttle reset`
	ErrCooldown    ErrorKind = "cooldown"    // cooling down after a GitHub rate limit
	ErrLimit       ErrorKind = "limit"       // local write interval or cap would wait too long
	ErrDuplicate   ErrorKind = "duplicate"   // identical write already succeeded recently
	ErrRateLimited ErrorKind = "ratelimited" // GitHub just rate limited this request
	ErrState       ErrorKind = "state"       // shared state could not be read or written
)

// Error is returned from RoundTrip when gh-throttle refuses a request or
// GitHub rate limits it. gh exits with code 75 when it sees one.
type Error struct {
	Kind    ErrorKind
	Reason  string
	RetryAt time.Time
	now     time.Time
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("gh-throttle: ")
	b.WriteString(e.Reason)
	switch {
	case e.Kind == ErrBreaker:
		b.WriteString("\nAll GitHub API calls from gh are stopped. A human must run `gh throttle reset` in an interactive terminal.")
		b.WriteString("\nDo NOT retry and do NOT try to work around this. Tell the user.")
	case e.Kind == ErrDuplicate:
		b.WriteString("\nThis exact request already succeeded. Do NOT send it again.")
	case e.Kind == ErrState:
		b.WriteString("\nRun `gh throttle status` for details.")
	case !e.RetryAt.IsZero():
		now := e.now
		if now.IsZero() {
			now = time.Now()
		}
		wait := e.RetryAt.Sub(now).Round(time.Second)
		if wait < time.Second {
			wait = time.Second
		}
		fmt.Fprintf(&b, "\nDo NOT retry before %s (in %s). Continue with other work or tell the user.",
			e.RetryAt.Local().Format("15:04:05"), wait)
	}
	return b.String()
}

// ExitCode is the process exit code gh uses for throttle errors (EX_TEMPFAIL).
const ExitCode = 75
