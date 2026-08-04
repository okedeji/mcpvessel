package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/cliout"
	"github.com/okedeji/mcpvessel/internal/daemon"
)

// psRecentFinished is how many finished runs the default view keeps under the
// live ones; the full history stays behind -a.
const psRecentFinished = 10

func newPsCmd() *cobra.Command {
	var all, jsonOut bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List running cages",
		Long: `List the agents the daemon is currently running.

ps talks to the daemon, so it needs one running. Each row is a run: its id,
the agent reference, its status, when it started, and what it cost. Live runs
sort first, newest first, over the ` + fmt.Sprint(psRecentFinished) + ` most recently finished; -a/--all shows
the full history. --json emits every run verbatim, including a serving run's
endpoint URL.`,
		Example: `  mcpvessel ps
  mcpvessel ps -a
  mcpvessel ps --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			runs, err := daemon.Dial(socket).ListRuns(cmd.Context())
			if err != nil {
				var unreachable *daemon.Unreachable
				if errors.As(err, &unreachable) {
					return fmt.Errorf("%w (the daemon is not running; start it with 'mcpvessel init')", err)
				}
				return err
			}
			if jsonOut {
				return writeJSON(cmd.OutOrStdout(), runs)
			}
			printRuns(cmd.OutOrStdout(), runs, all)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "show the full run history, not just live and recent runs")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}

// printRuns renders the ps table: live runs first, then finished ones, each
// newest first. Without all, finished runs past psRecentFinished are elided
// behind a trailer naming -a, so the live rows are never buried under history.
func printRuns(w io.Writer, runs []daemon.RunInfo, all bool) {
	if len(runs) == 0 {
		cliout.Empty(w, "No runs yet. Start one with 'mcpvessel run' or 'mcpvessel serve'.")
		return
	}
	var live, done []daemon.RunInfo
	for _, r := range runs {
		if isLive(r) {
			live = append(live, r)
		} else {
			done = append(done, r)
		}
	}
	newestFirst := func(rs []daemon.RunInfo) {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].StartedAt.After(rs[j].StartedAt) })
	}
	newestFirst(live)
	newestFirst(done)
	elided := 0
	if !all && len(done) > psRecentFinished {
		elided = len(done) - psRecentFinished
		done = done[:psRecentFinished]
	}
	rows := make([][]string, 0, len(live)+len(done))
	for _, r := range append(live, done...) {
		rows = append(rows, []string{r.ID, r.Ref, r.Status, since(r.StartedAt), cost(r.CostMicroUSD)})
	}
	cliout.Table(w, []string{"RUN ID", "REF", "STATUS", "STARTED", "COST"}, rows)
	if elided > 0 {
		_, _ = fmt.Fprintf(w, "... and %d older; 'mcpvessel ps -a' shows all\n", elided)
	}
}

// isLive reports whether a run still holds containers: attached and one-shot
// runs while their call is in flight, serve entries and instances until
// released.
func isLive(r daemon.RunInfo) bool {
	return r.Status == "running" || r.Status == "serving"
}

// cost is blank when nothing was metered, so a tool collection or an unstarted
// run does not show a misleading $0.0000.
func cost(microUSD int64) string {
	if microUSD == 0 {
		return ""
	}
	return "$" + formatUSDMicros(microUSD)
}

// since formats an age as a single coarse unit ("3s", "5m", "2h", "3d").
func since(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := nowFunc().Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// nowFunc is swapped in tests.
var nowFunc = time.Now
