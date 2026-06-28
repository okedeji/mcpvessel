package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/okedeji/agentcage/internal/runtime"
)

// startMetrics binds the Prometheus scrape endpoint on addr and returns a stop to
// defer, or nil when the address will not bind (warned, not fatal: a wedged
// metrics port must not stop the daemon serving runs).
func (d *Daemon) startMetrics(addr string) func() {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: metrics listener on %s: %v\n", addr, err)
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", d.handleMetrics)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	fmt.Fprintf(os.Stderr, "agentcage metrics on http://%s/metrics\n", addr)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// handleMetrics serves the daemon's Prometheus metrics: run counts by status and
// total spend from the history, plus live run and cage gauges. It is served on a
// separate TCP listener (Prometheus scrapes TCP, not the control socket), only
// when the operator sets telemetry.metrics_addr.
func (d *Daemon) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	d.writeMetrics(&b)
	_, _ = w.Write([]byte(b.String()))
}

func (d *Daemon) writeMetrics(b *strings.Builder) {
	byStatus := map[string]int{}
	var totalMicroUSD, totalTokens int64
	if d.hist != nil {
		if recs, err := d.hist.List(); err == nil {
			for _, r := range recs {
				byStatus[r.Status]++
				totalMicroUSD += r.CostMicroUSD
				totalTokens += r.TotalTokens
			}
		}
	}
	d.mu.Lock()
	liveRuns := len(d.runs)
	d.mu.Unlock()

	fmt.Fprintln(b, "# HELP agentcage_runs_total Runs recorded in the history, by status.")
	fmt.Fprintln(b, "# TYPE agentcage_runs_total counter")
	statuses := make([]string, 0, len(byStatus))
	for s := range byStatus {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)
	for _, s := range statuses {
		fmt.Fprintf(b, "agentcage_runs_total{status=%q} %d\n", s, byStatus[s])
	}

	fmt.Fprintln(b, "# HELP agentcage_runs_live Runs the daemon is currently holding.")
	fmt.Fprintln(b, "# TYPE agentcage_runs_live gauge")
	fmt.Fprintf(b, "agentcage_runs_live %d\n", liveRuns)

	fmt.Fprintln(b, "# HELP agentcage_cages_live Cages live across every run on this host.")
	fmt.Fprintln(b, "# TYPE agentcage_cages_live gauge")
	fmt.Fprintf(b, "agentcage_cages_live %d\n", runtime.LiveCages())

	fmt.Fprintln(b, "# HELP agentcage_cost_usd_total Metered LLM spend across recorded runs.")
	fmt.Fprintln(b, "# TYPE agentcage_cost_usd_total counter")
	fmt.Fprintf(b, "agentcage_cost_usd_total %.6f\n", float64(totalMicroUSD)/1e6)

	fmt.Fprintln(b, "# HELP agentcage_tokens_total Prompt plus completion tokens across recorded runs.")
	fmt.Fprintln(b, "# TYPE agentcage_tokens_total counter")
	fmt.Fprintf(b, "agentcage_tokens_total %d\n", totalTokens)
}
