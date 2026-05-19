package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

type runFlags struct {
	plan             string
	agent            string
	target           string
	ports            stringSliceFlag
	paths            stringSliceFlag
	skipPaths        stringSliceFlag
	tokenBudget      int64
	maxDuration      string
	maxTotalCages    int
	maxIterations    int
	context          string
	endpoints        stringSliceFlag
	apiSpecs         stringSliceFlag
	knownWeaknesses  stringSliceFlag
	limitToListed    bool
	autoApprovePlan  bool
	noPentestHeader  bool
	notify           string
	notifyOnFinding  bool
	notifyOnComplete bool
	follow           bool
	format           string
	name             string
	tags             stringSliceFlag
	customerID       string
}

func parseRunFlags(args []string) (*runFlags, *flag.FlagSet) {
	rf := &runFlags{}
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.Usage = printRunUsage

	fs.StringVar(&rf.plan, "plan", "", "path to assessment YAML plan file")
	fs.StringVar(&rf.agent, "agent", "", "agent name:tag or ref (e.g. agent-starter:latest)")
	fs.StringVar(&rf.target, "target", "", "target host (one per assessment)")
	fs.Var(&rf.ports, "port", "port to include (repeatable)")
	fs.Var(&rf.paths, "path", "URL path to scope (repeatable)")
	fs.Var(&rf.skipPaths, "skip-path", "URL path to skip (repeatable)")
	fs.Int64Var(&rf.tokenBudget, "token-budget", 0, "LLM token cap (0 = use server default)")
	fs.StringVar(&rf.maxDuration, "max-duration", "", "assessment wall clock (e.g. 30m, 4h)")
	fs.IntVar(&rf.maxTotalCages, "max-total-cages", 0, "total cage cap for the assessment (0 = use server default)")
	fs.IntVar(&rf.maxIterations, "max-iterations", 0, "max coordinator iterations (0 = use server default)")
	fs.StringVar(&rf.context, "context", "", "free-text context for the LLM coordinator")
	fs.Var(&rf.endpoints, "endpoint", "endpoint to focus on (repeatable)")
	fs.Var(&rf.apiSpecs, "api-spec", "OpenAPI/GraphQL spec URL (repeatable)")
	fs.Var(&rf.knownWeaknesses, "known-weakness", "known weakness hint (repeatable)")
	fs.BoolVar(&rf.limitToListed, "limit-to-listed", false, "test only the listed endpoints; ignore other discovery findings")
	fs.BoolVar(&rf.autoApprovePlan, "auto-approve-plan", false, "skip the human plan-approval gate after discovery (autonomous runs)")
	fs.BoolVar(&rf.noPentestHeader, "no-pentest-header", false, "skip the X-Agentcage-Pentest header on outbound requests (adversarial-simulation engagements)")
	fs.StringVar(&rf.notify, "notify", "", "webhook URL for notifications")
	fs.BoolVar(&rf.notifyOnFinding, "notify-on-finding", false, "notify per validated finding")
	fs.BoolVar(&rf.notifyOnComplete, "notify-on-complete", false, "notify when assessment finishes")
	fs.BoolVar(&rf.follow, "follow", false, "stream status updates until terminal state")
	fs.StringVar(&rf.format, "format", "text", "output format: text, json")
	fs.StringVar(&rf.name, "name", "", "human name for this assessment")
	fs.Var(&rf.tags, "tag", "key=value metadata (repeatable)")
	fs.StringVar(&rf.customerID, "customer-id", "", "customer identifier")

	_ = fs.Parse(args)
	return rf, fs
}

func explicitFlags(fs *flag.FlagSet) map[string]bool {
	m := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		m[f.Name] = true
	})
	return m
}

func printRunUsage() {
	fmt.Fprintf(os.Stderr, `usage: agentcage run --agent <ref> --target <host> [flags]
       agentcage run --plan <assessment.yaml> [flag overrides]

Examples:
  agentcage run --agent c9116254345e --target example.com --customer-id cust-1
  agentcage run --plan plans/staging.yaml --follow
  agentcage run --agent c9116254345e --target api.example.com --endpoint /api/auth --context "Django app, just rewrote OAuth"

Required (unless in plan file):
  --agent              agent name:tag or ref (e.g. agent-starter:latest)
  --target             target host (one per assessment)
  --customer-id        customer identifier

Plan file:
  --plan               path to assessment YAML plan file

Target scoping:
  --port               port to include (repeatable)
  --path               URL path to scope (repeatable)
  --skip-path          URL path to skip (repeatable)

Budget & limits:
  --token-budget       LLM token cap (0 = use server default)
  --max-duration       assessment wall clock (e.g. 30m, 4h)
  --max-total-cages    total cage cap for the assessment (0 = use server default)
  --max-iterations     max coordinator iterations (0 = use server default)

Guidance:
  --context            free-text context for the LLM coordinator
  --endpoint           endpoint to focus on (repeatable)
  --api-spec           OpenAPI/GraphQL spec URL (repeatable)
  --known-weakness     known weakness hint (repeatable)
  --limit-to-listed    test only the listed endpoints; ignore other discovery findings

Workflow:
  --auto-approve-plan  skip the human plan-approval gate after discovery (autonomous runs)
  --no-pentest-header  skip the X-Agentcage-Pentest header on outbound requests (adversarial-simulation engagements)

Notifications:
  --notify             webhook URL for notifications
  --notify-on-finding  notify per validated finding
  --notify-on-complete notify when assessment finishes

Output:
  --follow             stream status updates until terminal state
  --format             output format: text, json
  --name               human name for this assessment
  --tag                key=value metadata (repeatable)
`)
}
