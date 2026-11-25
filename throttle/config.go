// Package throttle paces gh's own GitHub API traffic so that automated callers
// (for example AI coding agents) cannot burst requests that get a personal
// account flagged. All of gh-throttle lives under this directory; upstream gh
// code only calls into it through three hooks:
//
//   - cmd/gh/main.go wraps the program in Main, which serves cacheable read
//     commands from a shared cache exactly as ghx does (see package ghx) and
//     maps throttle failures to exit code 75.
//   - api/http_client.go installs NewTransport as the innermost
//     http.RoundTripper, so every GitHub API request passes through it right
//     before it leaves the machine.
//   - pkg/cmd/root/root.go adds the `gh throttle` command.
//
// Writes are queued across all gh processes on the machine, spaced by a
// minimum interval, capped per minute and per hour, and identical writes
// inside a short window are refused. When GitHub signals a rate limit, every
// gh process cools down, and repeated secondary rate limits open a circuit
// breaker that only a human can close with `gh throttle reset`.
package throttle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	ghConfig "github.com/cli/go-gh/v2/pkg/config"
	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that reads and writes as a Go duration string
// such as "3s" or "1h" in YAML.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// Config holds the throttle limits. Defaults follow limits the community has
// measured for content creation (about 20 per minute and 150 per hour), which
// are lower than the documented 80 per minute and 500 per hour.
type Config struct {
	// WriteInterval is the minimum gap between two writes.
	WriteInterval Duration `yaml:"write_interval"`
	// WritesPerMinute caps writes in any rolling minute. 0 disables the cap.
	WritesPerMinute int `yaml:"writes_per_minute"`
	// WritesPerHour caps writes in any rolling hour. 0 disables the cap.
	WritesPerHour int `yaml:"writes_per_hour"`
	// DedupeWindow refuses an identical write that succeeded this recently.
	DedupeWindow Duration `yaml:"dedupe_window"`
	// MaxWait is how long a write may sleep for its turn before it is refused.
	MaxWait Duration `yaml:"max_wait"`
	// BreakerStrikes secondary rate limits within BreakerWindow open the breaker.
	BreakerStrikes int      `yaml:"breaker_strikes"`
	BreakerWindow  Duration `yaml:"breaker_window"`
	// CooldownBase is the first cooldown after a secondary rate limit; each
	// further strike inside BreakerWindow doubles it, up to CooldownMax.
	CooldownBase Duration `yaml:"cooldown_base"`
	CooldownMax  Duration `yaml:"cooldown_max"`
	// LockTimeout bounds how long a write waits for writes queued ahead of it.
	LockTimeout Duration `yaml:"lock_timeout"`

	// Read cache settings, with ghx's names and defaults.
	Cache CacheConfig `yaml:"cache"`
}

// CacheConfig mirrors ghx's config.yaml cache keys.
type CacheConfig struct {
	TTL             Duration            `yaml:"ttl"`
	TTLOverrides    map[string]Duration `yaml:"ttl_overrides"` // keyed like "pr_list"
	ImmutableTTL    Duration            `yaml:"immutable_ttl"` // content addressed by commit SHA
	SettledTTL      Duration            `yaml:"settled_ttl"`   // runs/jobs reporting status "completed"
	MaxCacheEntries int                 `yaml:"max_cache_entries"`
	MaxCacheBytes   int64               `yaml:"max_cache_bytes"`
	AdditionalCache []string            `yaml:"additional_cacheable"`
}

// DefaultConfig returns the built-in limits.
func DefaultConfig() Config {
	return Config{
		WriteInterval:   Duration{3 * time.Second},
		WritesPerMinute: 20,
		WritesPerHour:   150,
		DedupeWindow:    Duration{time.Minute},
		MaxWait:         Duration{time.Minute},
		BreakerStrikes:  3,
		BreakerWindow:   Duration{time.Hour},
		CooldownBase:    Duration{time.Minute},
		CooldownMax:     Duration{30 * time.Minute},
		LockTimeout:     Duration{5 * time.Minute},
		Cache: CacheConfig{
			TTL:             Duration{30 * time.Second},
			TTLOverrides:    map[string]Duration{},
			ImmutableTTL:    Duration{time.Hour},
			SettledTTL:      Duration{10 * time.Minute},
			MaxCacheEntries: 1000,
			MaxCacheBytes:   64 << 20,
		},
	}
}

func (c Config) validate() error {
	switch {
	case c.WriteInterval.Duration < 0, c.DedupeWindow.Duration < 0, c.MaxWait.Duration < 0,
		c.Cache.TTL.Duration < 0, c.CooldownBase.Duration < 0:
		return errors.New("durations must not be negative")
	case c.WritesPerMinute < 0, c.WritesPerHour < 0:
		return errors.New("write caps must not be negative")
	case c.BreakerStrikes < 1:
		return errors.New("breaker_strikes must be at least 1")
	case c.BreakerWindow.Duration <= 0:
		return errors.New("breaker_window must be positive")
	case c.CooldownMax.Duration < c.CooldownBase.Duration:
		return errors.New("cooldown_max must be at least cooldown_base")
	case c.LockTimeout.Duration <= 0:
		return errors.New("lock_timeout must be positive")
	}
	return nil
}

// Dir is where throttle keeps its shared state, cache and call log. It lives
// in gh's state directory rather than its cache directory, so
// `gh config clear-cache` does not wipe the rate-limit history.
func Dir() string { return filepath.Join(ghConfig.StateDir(), "throttle") }

// ConfigPath is the optional YAML file that overrides DefaultConfig.
func ConfigPath() string { return filepath.Join(ghConfig.ConfigDir(), "throttle.yml") }

// LoadConfig reads path on top of the defaults. A missing file is not an error.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return DefaultConfig(), fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return DefaultConfig(), fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}
