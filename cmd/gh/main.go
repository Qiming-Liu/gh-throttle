package main

import (
	"os"

	"github.com/cli/cli/v2/internal/ghcmd"
	"github.com/cli/cli/v2/throttle"
)

func main() {
	code := throttle.Main(func() int { return int(ghcmd.Main()) })
	os.Exit(code)
}
