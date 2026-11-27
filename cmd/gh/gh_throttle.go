package main

import (
	"os"

	"github.com/cli/cli/v2/internal/ghcmd"
	"github.com/cli/cli/v2/throttle"
)

// init runs gh under gh-throttle and exits, so main in main.go is never
// reached. Doing it here instead of in main keeps main.go identical to
// upstream, which keeps upstream syncs free of conflicts in that file.
//
// It is safe because every imported package has finished initializing before
// this init runs, and main.go has no package-level state of its own.
func init() {
	os.Exit(throttle.Main(func() int { return int(ghcmd.Main()) }))
}
