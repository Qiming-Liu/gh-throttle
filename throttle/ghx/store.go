package ghx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is a cached command result, as in ghx's cache.Entry.
type Entry struct {
	Key      string        `json:"key"`
	Stdout   []byte        `json:"stdout"`
	Stderr   []byte        `json:"stderr"`
	ExitCode int           `json:"exit_code"`
	CachedAt time.Time     `json:"cached_at"`
	TTL      time.Duration `json:"ttl"`
	Resource ResourceType  `json:"resource"`
	Host     string        `json:"host"`
	Repo     string        `json:"repo"`
}

func (e *Entry) expired(now time.Time) bool { return now.Sub(e.CachedAt) > e.TTL }

// Store is ghx's LRU cache with TTLs and namespace invalidation, kept on disk
// so every gh process shares it. Layout under dir:
//
//	e/<key>.json                          entry; mtime is its LRU position
//	n/<namespace>.<resource>.<key>        empty marker used for invalidation
//	f/<base key>.json                     --json field index (see Handler.indexFields in ghx)
//	l/<key>.lock                          singleflight lock
type Store struct {
	dir        string
	maxEntries int
	maxBytes   int64
	now        func() time.Time
}

func NewStore(dir string, maxEntries int, maxBytes int64) *Store {
	return &Store{dir: dir, maxEntries: maxEntries, maxBytes: maxBytes, now: time.Now}
}

func (s *Store) entryPath(key string) string { return filepath.Join(s.dir, "e", key+".json") }

func namespaceTag(host, repo string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(host) + "\x00" + strings.ToLower(repo)))
	return hex.EncodeToString(sum[:8])
}

func resourceTag(r ResourceType) string {
	if r == ResourceUnknown {
		return "_"
	}
	return string(r)
}

func (s *Store) markerPath(e *Entry) string {
	return filepath.Join(s.dir, "n", namespaceTag(e.Host, e.Repo)+"."+resourceTag(e.Resource)+"."+e.Key)
}

// Get returns a live entry, or nil.
func (s *Store) Get(key string) *Entry {
	e, _ := s.GetWithStatus(key)
	return e
}

// GetWithStatus also reports why a lookup missed, like ghx's cache.
func (s *Store) GetWithStatus(key string) (*Entry, string) {
	path := s.entryPath(key)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "absent"
	}
	var e Entry
	if json.Unmarshal(b, &e) != nil {
		_ = os.Remove(path)
		return nil, "absent"
	}
	now := s.now()
	if e.expired(now) {
		s.remove(&e)
		return nil, "expired"
	}
	_ = os.Chtimes(path, now, now) // most recently used
	return &e, "hit"
}

// Contains reports whether key holds a live entry.
func (s *Store) Contains(key string) bool {
	b, err := os.ReadFile(s.entryPath(key))
	if err != nil {
		return false
	}
	var e Entry
	return json.Unmarshal(b, &e) == nil && !e.expired(s.now())
}

// Set stores an entry, evicting least recently used entries to stay within
// the entry and byte limits. Oversized entries are rejected.
func (s *Store) Set(e *Entry) {
	if old := s.read(e.Key); old != nil {
		s.remove(old)
	}
	size := int64(len(e.Stdout)) + int64(len(e.Stderr))
	if s.maxEntries <= 0 || s.maxBytes <= 0 || size > s.maxBytes {
		return
	}
	for _, d := range []string{"e", "n"} {
		if err := os.MkdirAll(filepath.Join(s.dir, d), 0o700); err != nil {
			return
		}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	s.evictFor(size)
	path := s.entryPath(e.Key)
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	if os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.WriteFile(s.markerPath(e), nil, 0o600)
}

func (s *Store) read(key string) *Entry {
	b, err := os.ReadFile(s.entryPath(key))
	if err != nil {
		return nil
	}
	var e Entry
	if json.Unmarshal(b, &e) != nil {
		return nil
	}
	return &e
}

func (s *Store) remove(e *Entry) {
	_ = os.Remove(s.entryPath(e.Key))
	_ = os.Remove(s.markerPath(e))
}

// evictFor drops least recently used entries until one more entry of size
// bytes fits.
func (s *Store) evictFor(size int64) {
	dir := filepath.Join(s.dir, "e")
	des, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		key   string
		size  int64
		mtime time.Time
	}
	var items []item
	var total int64
	for _, de := range des {
		name := de.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		items = append(items, item{strings.TrimSuffix(name, ".json"), fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mtime.Before(items[j].mtime) })
	for len(items) > 0 && (len(items) >= s.maxEntries || total > s.maxBytes-size) {
		if e := s.read(items[0].key); e != nil {
			s.remove(e)
		} else {
			_ = os.Remove(s.entryPath(items[0].key))
		}
		total -= items[0].size
		items = items[1:]
	}
}

func (s *Store) removeMarkers(prefix string) int {
	des, err := os.ReadDir(filepath.Join(s.dir, "n"))
	if err != nil {
		return 0
	}
	n := 0
	for _, de := range des {
		name := de.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		parts := strings.SplitN(name, ".", 3)
		if len(parts) == 3 {
			_ = os.Remove(s.entryPath(parts[2]))
		}
		_ = os.Remove(filepath.Join(s.dir, "n", name))
		n++
	}
	return n
}

// InvalidateNamespace removes entries for host, repo and resource.
func (s *Store) InvalidateNamespace(host, repo string, resource ResourceType) int {
	return s.removeMarkers(namespaceTag(host, repo) + "." + resourceTag(resource) + ".")
}

// InvalidateRepo removes every entry for host and repo.
func (s *Store) InvalidateRepo(host, repo string) int {
	return s.removeMarkers(namespaceTag(host, repo) + ".")
}

// Flush removes everything.
func (s *Store) Flush() error { return os.RemoveAll(s.dir) }

// fieldEntry records a cached --json response and the fields it selected.
type fieldEntry struct {
	Key    string   `json:"key"`
	Fields []string `json:"fields"`
}

const maxFieldEntriesPerBase = 8

func (s *Store) fieldIndexPath(baseKey string) string {
	return filepath.Join(s.dir, "f", baseKey+".json")
}

// IndexFields remembers that cacheKey holds output for fields, so a request
// for a subset of those fields with the same base command can reuse it.
func (s *Store) IndexFields(baseKey, cacheKey string, fields []string) {
	if baseKey == "" {
		return
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "f"), 0o700); err != nil {
		return
	}
	entries := s.fieldIndex(baseKey)
	live := entries[:0]
	for _, e := range entries {
		if e.Key != cacheKey && s.Contains(e.Key) {
			live = append(live, e)
		}
	}
	live = append(live, fieldEntry{Key: cacheKey, Fields: fields})
	if len(live) > maxFieldEntriesPerBase {
		live = live[len(live)-maxFieldEntriesPerBase:]
	}
	b, err := json.Marshal(live)
	if err != nil {
		return
	}
	path := s.fieldIndexPath(baseKey)
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if os.WriteFile(tmp, b, 0o600) == nil && os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
	}
}

func (s *Store) fieldIndex(baseKey string) []fieldEntry {
	b, err := os.ReadFile(s.fieldIndexPath(baseKey))
	if err != nil {
		return nil
	}
	var out []fieldEntry
	_ = json.Unmarshal(b, &out)
	return out
}

// LookupSuperset finds a successful cached response for a superset of fields
// and projects it down to the requested fields.
func (s *Store) LookupSuperset(baseKey string, fields []string) *Entry {
	if baseKey == "" {
		return nil
	}
	candidates := s.fieldIndex(baseKey)
	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		if !Covers(c.Fields, fields) {
			continue
		}
		e := s.Get(c.Key)
		if e == nil || e.ExitCode != 0 {
			continue
		}
		if out, ok := Project(e.Stdout, fields); ok {
			return &Entry{Stdout: out, Stderr: e.Stderr}
		}
	}
	return nil
}

// LockPath is the singleflight lock for key.
func (s *Store) LockPath(key string) (string, error) {
	dir := filepath.Join(s.dir, "l")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, key+".lock"), nil
}

// Sweep removes expired entries, stale temp files and old lock files.
func (s *Store) Sweep() {
	now := s.now()
	if des, err := os.ReadDir(filepath.Join(s.dir, "e")); err == nil {
		for _, de := range des {
			name := de.Name()
			if strings.HasSuffix(name, ".tmp") {
				if fi, err := de.Info(); err == nil && now.Sub(fi.ModTime()) > time.Minute {
					_ = os.Remove(filepath.Join(s.dir, "e", name))
				}
				continue
			}
			if e := s.read(strings.TrimSuffix(name, ".json")); e != nil && e.expired(now) {
				s.remove(e)
			}
		}
	}
	if des, err := os.ReadDir(filepath.Join(s.dir, "l")); err == nil {
		for _, de := range des {
			if fi, err := de.Info(); err == nil && now.Sub(fi.ModTime()) > time.Hour {
				_ = os.Remove(filepath.Join(s.dir, "l", de.Name()))
			}
		}
	}
}
