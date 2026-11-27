package root

import (
	"github.com/cli/cli/v2/pkg/cmdutil"
	throttleCmd "github.com/cli/cli/v2/throttle/cmd"
	"github.com/spf13/cobra"
)

// newCmdThrottle returns `gh throttle`. It lives in its own file so that
// root.go only needs a one-line hook and no import, which keeps upstream
// syncs free of conflicts.
func newCmdThrottle(f *cmdutil.Factory) *cobra.Command {
	return throttleCmd.NewCmdThrottle(f)
}
