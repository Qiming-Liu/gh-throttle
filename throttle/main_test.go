package throttle

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseFlags(t *testing.T) {
	t.Setenv("GHX_NO_CACHE", "")
	args, noCache, ttl := parseFlags([]string{"--no-cache", "--ttl", "120", "pr", "list", "--no-cache"})
	assert.Equal(t, []string{"pr", "list", "--no-cache"}, args, "only leading flags are ghx flags")
	assert.True(t, noCache)
	assert.Equal(t, 2*time.Minute, ttl)

	args, noCache, ttl = parseFlags([]string{"pr", "list"})
	assert.Equal(t, []string{"pr", "list"}, args)
	assert.False(t, noCache)
	assert.Zero(t, ttl)
}

func TestExitCodeMapsTrippedFailures(t *testing.T) {
	tripped.Store(false)
	t.Cleanup(func() { tripped.Store(false) })
	assert.Equal(t, 1, exitCode(1))
	tripped.Store(true)
	assert.Equal(t, ExitCode, exitCode(1))
	assert.Equal(t, 0, exitCode(0), "a command that recovered still succeeds")
}
