package agentfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/okedeji/agentcage/internal/env"
)

// LLM providers the runtime knows in v0.
var validProviders = map[string]ModelProvider{
	"openai":    ProviderOpenAI,
	"anthropic": ProviderAnthropic,
}

func parse(r io.Reader) (*Agentfile, error) {
	af := &Agentfile{
		Env:  make(map[string]string),
		Meta: make(map[string]string),
	}
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := parseLine(af, line, lineNo); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading Agentfile: %w", err)
	}
	if err := validate(af); err != nil {
		return nil, err
	}
	return af, nil
}

func parseLine(af *Agentfile, line string, lineNo int) error {
	directive, rest := splitDirective(line)
	switch strings.ToUpper(directive) {
	case "FROM":
		return parseFrom(af, rest, lineNo)
	case "ENTRYPOINT":
		return parseEntrypoint(af, rest, lineNo)
	case "RUN":
		return parseRun(af, rest, lineNo)
	case "MODEL":
		return parseModel(af, rest, lineNo)
	case "MAIN":
		return parseMain(af, rest, lineNo)
	case "EXPOSE":
		return parseExpose(af, rest, lineNo)
	case "USES":
		return parseUses(af, rest, lineNo)
	case "BUDGET":
		return parseBudget(af, rest, lineNo)
	case "ENV":
		return parseEnv(af, rest, lineNo)
	case "SECRETS":
		return parseSecrets(af, rest, lineNo)
	case "NETWORK":
		return parseNetwork(af, rest, lineNo)
	case "META":
		return parseMeta(af, rest, lineNo)
	case "EVAL":
		return parseEval(af, rest, lineNo)
	default:
		return fmt.Errorf("line %d: unknown directive %q", lineNo, directive)
	}
}

func splitDirective(line string) (string, string) {
	for i, r := range line {
		if r == ' ' || r == '\t' {
			return line[:i], strings.TrimSpace(line[i+1:])
		}
	}
	return line, ""
}

func parseFrom(af *Agentfile, rest string, lineNo int) error {
	if af.From != "" {
		return fmt.Errorf("line %d: FROM declared twice", lineNo)
	}
	if rest == "" {
		return fmt.Errorf("line %d: FROM requires an OCI image reference", lineNo)
	}
	af.From = rest
	return nil
}

func parseEntrypoint(af *Agentfile, rest string, lineNo int) error {
	if af.Entrypoint != "" {
		return fmt.Errorf("line %d: ENTRYPOINT declared twice", lineNo)
	}
	if rest == "" {
		return fmt.Errorf("line %d: ENTRYPOINT requires a command line", lineNo)
	}
	af.Entrypoint = rest
	return nil
}

func parseRun(af *Agentfile, rest string, lineNo int) error {
	if rest == "" {
		return fmt.Errorf("line %d: RUN requires a command", lineNo)
	}
	af.Run = append(af.Run, rest)
	return nil
}

func parseModel(af *Agentfile, rest string, lineNo int) error {
	if af.Model != nil {
		return fmt.Errorf("line %d: MODEL declared twice", lineNo)
	}
	if rest == "" {
		return fmt.Errorf("line %d: MODEL requires provider/model-name", lineNo)
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("line %d: MODEL must be provider/model-name (got %q)", lineNo, rest)
	}
	provider, ok := validProviders[parts[0]]
	if !ok {
		return fmt.Errorf("line %d: unknown provider %q (v0 supports openai, anthropic)", lineNo, parts[0])
	}
	af.Model = &Model{Provider: provider, Name: parts[1]}
	return nil
}

// parseMain handles the MAIN directive, which names the tool that
// runs when the agent is invoked as an agent (`agentcage run BUNDLE
// "..."`). The validator does NOT confirm the tool actually exists in
// the agent's MCP server. That check belongs to the build-time
// introspection pass (M2 work). Here we only validate the surface
// shape: one token, declared at most once.
func parseMain(af *Agentfile, rest string, lineNo int) error {
	if af.Main != "" {
		return fmt.Errorf("line %d: MAIN declared twice", lineNo)
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return fmt.Errorf("line %d: MAIN requires a tool name", lineNo)
	}
	if len(fields) > 1 {
		return fmt.Errorf("line %d: MAIN takes a single tool name (got %q)", lineNo, rest)
	}
	af.Main = fields[0]
	return nil
}

// parseExpose handles the EXPOSE directive. Repeatable. Each invocation
// adds one or more tool names to the agent's public surface. Tools
// not in Expose (and not equal to Main) stay private. Duplicate names
// are silently deduplicated so authors can be redundant across lines
// without breaking the build.
func parseExpose(af *Agentfile, rest string, lineNo int) error {
	if rest == "" {
		return fmt.Errorf("line %d: EXPOSE requires at least one tool name", lineNo)
	}
	rest = strings.ReplaceAll(rest, ",", " ")
	seen := make(map[string]struct{}, len(af.Expose))
	for _, name := range af.Expose {
		seen[name] = struct{}{}
	}
	for _, name := range strings.Fields(rest) {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		af.Expose = append(af.Expose, name)
	}
	return nil
}

func parseUses(af *Agentfile, rest string, lineNo int) error {
	if rest == "" {
		return fmt.Errorf("line %d: USES requires a reference", lineNo)
	}
	public := false
	if upper := strings.ToUpper(rest); strings.HasPrefix(upper, "PUBLIC ") {
		public = true
		rest = strings.TrimSpace(rest[len("PUBLIC"):])
	}

	// USES <ref> [DENY tool1,tool2 ...]
	// Split on whitespace: first token is the ref, remaining tokens are
	// the optional DENY clause.
	parts := strings.Fields(rest)
	if len(parts) == 0 {
		return fmt.Errorf("line %d: USES requires a reference", lineNo)
	}
	use, err := parseUseRef(parts[0], lineNo)
	if err != nil {
		return err
	}
	use.Public = public

	if len(parts) > 1 {
		if !strings.EqualFold(parts[1], "DENY") {
			return fmt.Errorf("line %d: USES expected DENY after reference, got %q", lineNo, parts[1])
		}
		if len(parts) < 3 {
			return fmt.Errorf("line %d: USES DENY requires at least one tool name", lineNo)
		}
		deny := parseDenyList(strings.Join(parts[2:], " "))
		if len(deny) == 0 {
			return fmt.Errorf("line %d: USES DENY requires at least one non-empty tool name", lineNo)
		}
		use.Deny = deny
	}

	af.Uses = append(af.Uses, use)
	return nil
}

// parseDenyList splits a DENY clause's tail into a deduped list of
// tool names. Accepts commas, spaces, or tabs as separators so an
// author can write `DENY a,b,c` or `DENY a b c` and get the same
// result. Empty entries (from `a,,b`) are dropped.
func parseDenyList(s string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, raw := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func parseUseRef(ref string, lineNo int) (Use, error) {
	if !strings.HasPrefix(ref, "@") {
		return Use{}, fmt.Errorf("line %d: USES reference must start with @ (got %q)", lineNo, ref)
	}
	colon := strings.LastIndex(ref, ":")
	if colon == -1 {
		return Use{}, fmt.Errorf("line %d: USES reference must include a version tag like @org/name:1.2.0 (got %q)", lineNo, ref)
	}
	name := ref[:colon]
	version := ref[colon+1:]
	if version == "" {
		return Use{}, fmt.Errorf("line %d: USES reference has an empty version tag", lineNo)
	}
	if version == "latest" {
		// latest is too ambiguous for shippable bundles: the tag points
		// at different content over time, breaking reproducibility.
		return Use{}, fmt.Errorf("line %d: USES reference cannot use the latest tag", lineNo)
	}
	if !strings.Contains(name[1:], "/") {
		return Use{}, fmt.Errorf("line %d: USES reference must be @org/name:version (got %q)", lineNo, ref)
	}
	return Use{Ref: name, Version: version}, nil
}

func parseBudget(af *Agentfile, rest string, lineNo int) error {
	if af.Budget != 0 {
		return fmt.Errorf("line %d: BUDGET declared twice", lineNo)
	}
	fields := strings.Fields(rest)
	if len(fields) != 1 {
		return fmt.Errorf("line %d: BUDGET takes a single token count like 100000", lineNo)
	}
	tokens, err := strconv.Atoi(fields[0])
	if err != nil {
		return fmt.Errorf("line %d: BUDGET %q is not a token count", lineNo, fields[0])
	}
	if tokens <= 0 {
		return fmt.Errorf("line %d: BUDGET must be positive", lineNo)
	}
	af.Budget = tokens
	return nil
}

func parseEnv(af *Agentfile, rest string, lineNo int) error {
	parts := strings.SplitN(rest, "=", 2)
	if len(parts) != 2 || parts[0] == "" {
		return fmt.Errorf("line %d: ENV must be KEY=VALUE", lineNo)
	}
	if strings.HasPrefix(parts[0], env.Prefix) {
		return fmt.Errorf("line %d: ENV key %q uses reserved %s prefix", lineNo, parts[0], env.Prefix)
	}
	af.Env[parts[0]] = parts[1]
	return nil
}

func parseSecrets(af *Agentfile, rest string, lineNo int) error {
	if rest == "" {
		return fmt.Errorf("line %d: SECRETS requires at least one key", lineNo)
	}
	rest = strings.ReplaceAll(rest, ",", " ")
	af.Secrets = append(af.Secrets, strings.Fields(rest)...)
	return nil
}

func parseNetwork(af *Agentfile, rest string, lineNo int) error {
	if af.Network != "" {
		return fmt.Errorf("line %d: NETWORK declared twice", lineNo)
	}
	if rest == "" {
		return fmt.Errorf("line %d: NETWORK requires a policy", lineNo)
	}
	if rest != "deny-default" && !strings.HasPrefix(rest, "allow:") {
		return fmt.Errorf("line %d: NETWORK must be deny-default or allow:<domains> (got %q)", lineNo, rest)
	}
	af.Network = rest
	return nil
}

func parseMeta(af *Agentfile, rest string, lineNo int) error {
	key, value, err := splitMetaKV(rest)
	if err != nil {
		return fmt.Errorf("line %d: %w", lineNo, err)
	}
	af.Meta[key] = value
	return nil
}

func splitMetaKV(rest string) (string, string, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", "", errors.New("META requires key value")
	}
	parts := strings.SplitN(rest, " ", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", "", errors.New("META requires key value")
	}
	value := strings.TrimSpace(parts[1])
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	return parts[0], value, nil
}

func parseEval(af *Agentfile, rest string, lineNo int) error {
	if af.Eval != "" {
		return fmt.Errorf("line %d: EVAL declared twice", lineNo)
	}
	if rest == "" {
		return fmt.Errorf("line %d: EVAL requires a path", lineNo)
	}
	af.Eval = rest
	return nil
}

func validate(af *Agentfile) error {
	if af.From == "" {
		return errors.New("FROM is required")
	}
	if af.Entrypoint == "" {
		return errors.New("ENTRYPOINT is required")
	}
	return nil
}
