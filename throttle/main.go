package throttle

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cli/cli/v2/throttle/ghx"
	"github.com/mattn/go-isatty"
)

// childEnv marks a gh process started by the read cache to fetch a result.
// It only turns off command caching in that child; HTTP throttling still
// applies to every request it makes.
const childEnv = "GH_THROTTLE_CACHE_FETCH"

// Main is the gh-throttle entry point, called from cmd/gh/main.go with the
// function that runs gh in this process. It serves cacheable read commands
// from the shared ghx cache, invalidates the cache after mutations, and turns
// throttle failures into ExitCode.
func Main(run func() int) int {
	if os.Getenv(childEnv) == "1" || len(os.Args) < 2 {
		return exitCode(run())
	}

	// ghx's own flags may precede the gh command: --no-cache, --ttl <seconds>.
	ghArgs, noCache, ttlOverride := parseFlags(os.Args[1:])
	os.Args = append([]string{os.Args[0]}, ghArgs...)
	if len(ghArgs) == 0 {
		return exitCode(run())
	}

	cfg, err := LoadConfig(ConfigPath())
	if err != nil {
		return exitCode(run()) // the transport refuses every request until the config is fixed
	}
	self, err := os.Executable()
	if err != nil {
		return exitCode(run())
	}
	runner := newRunner(cfg, self)
	if rand.IntN(50) == 0 {
		defer runner.Store().Sweep()
	}

	cl := runner.Classify(ghArgs)
	switch cl.Type {
	case ghx.Mutation:
		ctx := ghx.Resolve(self)
		code := exitCode(run())
		// Invalidate even after a failure, which may have partly changed state.
		runner.Invalidate(cl, ctx)
		return code
	case ghx.Cacheable:
		if !cacheableOutput() {
			return exitCode(run())
		}
		res := runner.Cached(ghArgs, ghx.Resolve(self), cl, noCache, ttlOverride)
		_, _ = os.Stdout.Write(res.Stdout)
		_, _ = os.Stderr.Write(res.Stderr)
		return res.ExitCode
	default:
		return exitCode(run())
	}
}

func exitCode(code int) int {
	if code != 0 && tripped.Load() {
		return ExitCode
	}
	return code
}

// parseFlags extracts ghx's flags from the start of args, like ghx's
// parseGHXFlags. GHX_NO_CACHE=1 also skips cache lookups.
func parseFlags(args []string) (ghArgs []string, noCache bool, ttl time.Duration) {
	noCache = os.Getenv("GHX_NO_CACHE") == "1"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--no-cache":
			noCache = true
		case "--ttl":
			if i+1 < len(args) {
				i++
				if v, err := strconv.Atoi(args[i]); err == nil {
					ttl = time.Duration(v) * time.Second
				}
			}
		default:
			return args[i:], noCache, ttl
		}
	}
	return nil, noCache, ttl
}

// cacheableOutput reports whether this invocation's output can be served
// from the cache. ghx always runs cached commands without a terminal, so its
// cached output is gh's non-TTY output; gh-throttle only caches when the
// caller would get that same output anyway, which is the case for agents and
// scripts. Forced color or TTY output is never cached.
func cacheableOutput() bool {
	fd := os.Stdout.Fd()
	if isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd) {
		return false
	}
	for _, v := range []string{"GH_FORCE_TTY", "CLICOLOR_FORCE"} {
		if s := os.Getenv(v); s != "" && s != "0" {
			return false
		}
	}
	return true
}

func newRunner(cfg Config, self string) *ghx.Runner {
	overrides := map[string]time.Duration{}
	for k, v := range cfg.Cache.TTLOverrides {
		overrides[k] = v.Duration
	}
	opts := ghx.Options{
		Dir:             filepath.Join(Dir(), "ghx"),
		TTL:             cfg.Cache.TTL.Duration,
		TTLOverrides:    overrides,
		ImmutableTTL:    cfg.Cache.ImmutableTTL.Duration,
		SettledTTL:      cfg.Cache.SettledTTL.Duration,
		MaxCacheEntries: cfg.Cache.MaxCacheEntries,
		MaxCacheBytes:   cfg.Cache.MaxCacheBytes,
		Additional:      cfg.Cache.AdditionalCache,
		LockTimeout:     2 * time.Minute,
		// Never replay a throttle refusal or rate limit from the cache.
		Storable: func(r *ghx.Result) bool { return r.ExitCode != ExitCode },
	}
	return ghx.NewRunner(opts, func(args []string) *ghx.Result { return execChild(self, args) })
}

// execChild runs gh in a child process without a terminal and captures its
// output, as ghx's daemon does.
func execChild(self string, args []string) *ghx.Result {
	cmd := exec.Command(self, args...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			fmt.Fprintf(&stderr, "gh-throttle: %v\n", err)
			code = 1
		}
	}
	return &ghx.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: code}
}
