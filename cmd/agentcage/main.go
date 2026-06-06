package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags:
//
//	go build -ldflags "-X main.version=v0.0.1" ./cmd/agentcage/
var version = "v0-dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Printf("agentcage %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		fmt.Fprintln(os.Stderr, "v0 is in development. See DESIGN.md for the target spec.")
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`agentcage %s (v0 in development)

Usage: agentcage <command>

Commands:
  version    Print version
  help       Print this help

This branch is the v0 redesign in progress. The implementation milestones
(build, run, push, pull, daemon, serve, eval, etc.) are being added one by
one. See DESIGN.md and .claude/skills/agentcage-v0/SKILL.md for the spec.
`, version)
}
