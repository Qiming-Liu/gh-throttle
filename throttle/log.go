package throttle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const maxLogSize = 10 << 20

// logEntry is one line of calls.jsonl. Request bodies and query strings are
// never logged; writes carry only a short hash of their content.
type logEntry struct {
	Time     time.Time `json:"time"`
	PID      int       `json:"pid"`
	Kind     Kind      `json:"kind"`
	Method   string    `json:"method"`
	Host     string    `json:"host"`
	Path     string    `json:"path"`
	Outcome  string    `json:"outcome"` // sent, cache, blocked, ratelimited, error
	Status   int       `json:"status,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Hash     string    `json:"hash,omitempty"`
	Waited   int64     `json:"waited_ms,omitempty"`
	Duration int64     `json:"duration_ms"`
	Agent    string    `json:"agent,omitempty"`
}

// LogPath is the JSON-lines call log.
func LogPath(dir string) string { return filepath.Join(dir, "calls.jsonl") }

func appendLog(dir string, e logEntry) {
	path := LogPath(dir)
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogSize {
		_ = os.Rename(path, path+".1")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
