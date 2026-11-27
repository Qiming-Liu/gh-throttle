package api

import (
	"net/http"

	"github.com/cli/cli/v2/throttle"
)

// throttleTransport is the innermost transport of every GitHub API client, so
// gh-throttle paces each request right before it leaves the machine. It lives
// in its own file so that http_client.go only needs a one-line hook and no
// import, which keeps upstream syncs free of conflicts.
func throttleTransport() http.RoundTripper {
	return throttle.NewTransport(http.DefaultTransport)
}
