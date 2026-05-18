package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	pb "github.com/okedeji/agentcage/api/proto"
	"github.com/okedeji/agentcage/internal/assessment"
	"github.com/okedeji/agentcage/internal/config"
)

func cmdReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	fs.Usage = printReportUsage
	assessmentID := fs.String("assessment", "", "assessment ID (required)")
	format := fs.String("format", "text", "output format: text, json")
	output := fs.String("o", "", "write output to file instead of stdout")
	_ = fs.Parse(args)

	if *assessmentID == "" {
		fmt.Fprintln(os.Stderr, "error: --assessment is required")
		printReportUsage()
		os.Exit(1)
	}

	switch *format {
	case "text", "json":
	default:
		fmt.Fprintf(os.Stderr, "error: unknown format %q (supported: text, json)\n", *format)
		os.Exit(1)
	}

	cfg := config.Defaults()
	if resolved := config.Resolve(""); resolved != "" {
		override, err := config.Load(resolved)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: loading config: %v\n", err)
			os.Exit(1)
		}
		cfg = config.Merge(cfg, override)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := dialOrchestrator(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	client := pb.NewAssessmentServiceClient(conn)
	resp, err := client.GetReport(ctx, &pb.GetReportRequest{AssessmentId: *assessmentID})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	var report assessment.Report
	if err := json.Unmarshal(resp.GetReportJson(), &report); err != nil {
		fmt.Fprintf(os.Stderr, "error: parsing report: %v\n", err)
		os.Exit(1)
	}

	var out []byte
	switch *format {
	case "json":
		out, err = assessment.FormatJSON(&report)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "text":
		out = formatReportText(&report)
	}

	if *output != "" {
		if err := os.WriteFile(*output, out, 0600); err != nil {
			fmt.Fprintf(os.Stderr, "error: writing to %s: %v\n", *output, err)
			os.Exit(1)
		}
		fmt.Printf("Report written to %s\n", *output)
		return
	}

	fmt.Print(string(out))
}

func formatReportText(r *assessment.Report) []byte {
	var b []byte
	w := func(format string, args ...any) {
		b = append(b, []byte(fmt.Sprintf(format, args...))...)
	}

	w("Assessment Report: %s\n", r.AssessmentID)
	w("Customer:          %s\n", r.CustomerID)
	if r.GeneratedAt.IsZero() {
		w("Generated:         (not set)\n")
	} else {
		w("Generated:         %s\n", r.GeneratedAt.Format(time.RFC3339))
	}
	w("Status:            %s\n\n", r.Status)

	if r.ExecutiveSummary != "" {
		w("EXECUTIVE SUMMARY\n\n%s\n\n", r.ExecutiveSummary)
	}

	if r.Methodology != "" {
		w("METHODOLOGY\n\n%s\n\n", r.Methodology)
	}

	w("SUMMARY\n\n")
	w("  Total: %d findings\n", r.Summary.TotalFindings)
	w("  Critical: %d  High: %d  Medium: %d  Low: %d  Info: %d\n\n",
		r.Summary.Critical, r.Summary.High, r.Summary.Medium, r.Summary.Low, r.Summary.Info)

	w("FINDINGS\n\n")
	for i, f := range r.Findings {
		w("  %d. [%s/%s] %s\n", i+1, f.Severity, f.Status, f.Title)
		if f.CWE != "" {
			w("     CWE: %s", f.CWE)
			if f.CVSSScore > 0 {
				w("  CVSS: %.1f", f.CVSSScore)
			}
			w("\n")
		}
		w("     Vuln Class: %s\n", f.VulnClass)
		w("     Endpoint:   %s\n", f.Endpoint)
		if f.Description != "" {
			w("     %s\n", f.Description)
		}
		if f.Evidence != "" {
			w("     Evidence: %s\n", f.Evidence)
		}
		if f.ValidationProof != "" {
			w("     Proof: %s\n", f.ValidationProof)
		}
		if f.Remediation != "" {
			w("     Remediation: %s\n", f.Remediation)
		}
		w("\n")
	}

	if r.RemediationRoadmap != "" {
		w("REMEDIATION ROADMAP\n\n%s\n", r.RemediationRoadmap)
	}

	return b
}

func printReportUsage() {
	fmt.Fprintf(os.Stderr, `usage: agentcage report --assessment <id> [flags]

Show or export the assessment report.

Examples:
  agentcage report --assessment <assessment-id>
  agentcage report --assessment <assessment-id> --format json
  agentcage report --assessment <assessment-id> --format json -o report.json

Flags:
  --assessment   assessment ID (required)
  --format       output format: text, json (default: text)
  -o             write output to file instead of stdout
`)
}
