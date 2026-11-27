package throttle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
)

// Kind is whether a request reads or changes data on GitHub.
type Kind string

const (
	KindRead  Kind = "read"
	KindWrite Kind = "write"
)

// maxBuffer bounds how much of a request or response body is held in memory.
const maxBuffer = 8 << 20

type requestInfo struct {
	kind     Kind
	graphql  bool
	resource string // primary rate-limit bucket: core, graphql, search...
	path     string // URL path without a GHES "/api/v3" prefix
	hash     string // identifies a write for deduplication
}

// governed reports whether a request goes to a GitHub API host and should be
// throttled. OAuth endpoints on the web host and loopback test servers pass
// straight through.
func governed(req *http.Request) bool {
	host := strings.ToLower(req.URL.Hostname())
	if host == "localhost" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	if strings.HasPrefix(host, "api.") || strings.HasPrefix(host, "uploads.") {
		return true
	}
	// GitHub Enterprise Server serves its API under /api/ on the web host.
	return strings.HasPrefix(req.URL.Path, "/api/")
}

func apiPath(p string) string {
	for _, prefix := range []string{"/api/v3", "/api/uploads", "/api"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return strings.TrimPrefix(p, prefix)
		}
	}
	return p
}

var mutationRE = regexp.MustCompile(`(^|[^A-Za-z0-9_])mutation([^A-Za-z0-9_]|$)`)

// isMutation reports whether a GraphQL request body may change data. Anything
// it cannot parse counts as a mutation, so mistakes err towards throttling.
func isMutation(body []byte) bool {
	var q struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &q); err != nil || q.Query == "" {
		return true
	}
	var b strings.Builder
	for _, line := range strings.Split(q.Query, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return mutationRE.MatchString(b.String())
}

// bufferBody reads up to maxBuffer bytes of the request body and puts a
// replayable body back on req. It returns what it read and whether that was
// the whole body.
func bufferBody(req *http.Request) ([]byte, bool, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, true, nil
	}
	b, err := io.ReadAll(io.LimitReader(req.Body, maxBuffer+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) <= maxBuffer {
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(b))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
		return b, true, nil
	}
	rest := req.Body
	req.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), rest), rest}
	req.GetBody = nil
	return b[:maxBuffer], false, nil
}

func inspect(req *http.Request) (*requestInfo, error) {
	info := &requestInfo{path: apiPath(req.URL.Path)}
	info.graphql = info.path == "/graphql"

	body, complete, err := bufferBody(req)
	if err != nil {
		return nil, err
	}

	switch strings.ToUpper(req.Method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		info.kind = KindRead
	default:
		if info.graphql && complete && !isMutation(body) {
			info.kind = KindRead
		} else {
			info.kind = KindWrite
		}
	}

	switch {
	case info.graphql:
		info.resource = "graphql"
	case strings.HasPrefix(info.path, "/search/code"):
		info.resource = "code_search"
	case strings.HasPrefix(info.path, "/search/"):
		info.resource = "search"
	default:
		info.resource = "core"
	}

	if info.kind == KindWrite {
		// Writes from different accounts are different writes.
		h := sha256.New()
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s\x00", strings.ToUpper(req.Method), req.URL.String(), req.ContentLength, req.Header.Get("Authorization"))
		h.Write(body)
		info.hash = hex.EncodeToString(h.Sum(nil))
	}
	return info, nil
}
