package throttlecmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/throttle"
	"github.com/cli/go-gh/v2/pkg/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func NewCmdThrottle(f *cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "throttle <command>",
		Short: "Inspect and reset GitHub API throttling",
		Long: heredoc.Doc(`
			gh paces its own GitHub API traffic so automated callers cannot burst
			requests that get an account flagged.

			Writes from every gh process on this machine are queued, spaced by a
			minimum interval, capped per minute and per hour, and an identical
			write that succeeded within the dedupe window is refused. Successful
			reads are shared for a few seconds. When GitHub reports a rate limit,
			all gh processes cool down; repeated secondary rate limits open a
			circuit breaker that only a human can close with "gh throttle reset".

			Refused requests exit with status 75.

			Limits are set in the throttle.yml file in gh's config directory.
		`),
	}
	cmdutil.DisableAuthCheck(cmd)

	cmd.AddCommand(newCmdStatus(f))
	cmd.AddCommand(newCmdReset(f))
	cmd.AddCommand(newCmdLog(f))
	cmd.AddCommand(newCmdConfig(f))
	return cmd
}

func newCmdStatus(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show breaker, cooldowns and write budget",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			io := f.IOStreams
			cs := io.ColorScheme()
			st, err := throttle.NewManager().Status(cmd.Context())
			if err != nil {
				return err
			}
			out := io.Out
			now := time.Now()
			if st.ConfigErr != nil {
				fmt.Fprintf(out, "%s config error: %v (all API calls are refused until fixed)\n", cs.FailureIcon(), st.ConfigErr)
			}
			if st.BreakerOpen {
				fmt.Fprintf(out, "%s Circuit breaker OPEN since %s\n  %s\n  Run `gh throttle reset` to resume.\n",
					cs.FailureIcon(), st.BreakerAt.Local().Format(time.DateTime), st.BreakerReason)
			} else {
				fmt.Fprintf(out, "%s Circuit breaker closed\n", cs.SuccessIcon())
			}
			if len(st.Cooldowns) == 0 {
				fmt.Fprintln(out, "Cooldowns: none")
			} else {
				fmt.Fprintln(out, "Cooldowns:")
				for _, c := range st.Cooldowns {
					fmt.Fprintf(out, "  %-20s until %s (%s left)\n", c.Scope, c.Until.Local().Format("15:04:05"), c.Until.Sub(now).Round(time.Second))
				}
			}
			cfg := st.Config
			fmt.Fprintf(out, "Rate-limit strikes: %d of %d within %s\n", st.Strikes, cfg.BreakerStrikes, cfg.BreakerWindow.Duration)
			fmt.Fprintf(out, "Writes: %d/%s last minute, %d/%s last hour\n",
				st.WritesMinute, capString(cfg.WritesPerMinute), st.WritesHour, capString(cfg.WritesPerHour))
			if st.NextWrite > 0 {
				fmt.Fprintf(out, "Next write allowed in %s (%s)\n", st.NextWrite.Round(time.Second), st.NextWriteWhy)
			} else {
				fmt.Fprintln(out, "Next write allowed now")
			}
			fmt.Fprintf(out, "State: %s\nConfig: %s\n", st.Dir, st.ConfigPath)
			return nil
		},
	}
}

func capString(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprint(n)
}

func newCmdReset(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Close the circuit breaker and clear cooldowns (interactive only)",
		Long: heredoc.Doc(`
			Close the circuit breaker, clear cooldowns and rate-limit strikes, and
			clear gh's HTTP caches, which may hold responses recorded while rate
			limited. Write history is kept, so the per-minute and per-hour caps
			still apply.

			This must be run by a person in an interactive terminal.
		`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			io := f.IOStreams
			if !io.CanPrompt() {
				return errors.New("gh throttle reset must be run by a person in an interactive terminal")
			}
			fmt.Fprint(io.ErrOut, "GitHub rate limited this account. Make sure whatever caused it has stopped.\nType \"reset\" to resume API calls: ")
			line, _ := bufio.NewReader(io.In).ReadString('\n')
			if strings.TrimSpace(line) != "reset" {
				return cmdutil.CancelError
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			if err := throttle.NewManager().Reset(ctx, config.CacheDir()); err != nil {
				return err
			}
			fmt.Fprintf(io.Out, "%s Throttle reset\n", io.ColorScheme().SuccessIcon())
			return nil
		},
	}
}

func newCmdLog(f *cmdutil.Factory) *cobra.Command {
	var lines int
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show recent GitHub API calls made by gh",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path := throttle.NewManager().LogFile()
			b, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(f.IOStreams.ErrOut, "no calls logged yet")
				return nil
			}
			if err != nil {
				return err
			}
			all := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			if lines > 0 && len(all) > lines {
				all = all[len(all)-lines:]
			}
			for _, l := range all {
				fmt.Fprintln(f.IOStreams.Out, l)
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 20, "Number of most recent calls to show (0 for all)")
	return cmd
}

func newCmdConfig(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show the effective throttle limits",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path := throttle.ConfigPath()
			cfg, err := throttle.LoadConfig(path)
			if err != nil {
				return err
			}
			b, err := yaml.Marshal(cfg)
			if err != nil {
				return err
			}
			fmt.Fprintf(f.IOStreams.ErrOut, "# %s\n", path)
			_, err = f.IOStreams.Out.Write(b)
			return err
		},
	}
}
