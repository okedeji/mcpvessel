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
	case "BAN":
		return parseBan(af, rest, lineNo)
	case "BUDGET":
		return parseBudget(af, rest, lineNo)
	case "RESOURCES":
		return parseResources(af, rest, lineNo)
	case "ENV":
		return parseEnv(af, rest, lineNo)
	case "SECRETS":
		return parseSecrets(af, rest, lineNo)
	case "EGRESS":
		return parseEgress(af, rest, lineNo)
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
	af.Model = &Model{Provider: parts[0], Name: parts[1]}
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

// parseBan records an agent the root forbids anywhere in its subtree. An
// ONLY clause narrows the ban to specific tools; without it, the whole agent
// is banned. The ref is by name only, no version: a BAN takes out the agent
// however deep it appears and whatever version a dependency pinned, so a tag
// would be a foot-gun (you would ban one version and miss the next).
//
// BAN @org/name              bans the whole agent
// BAN @org/name ONLY t1,t2   bans those tools of that agent, subtree-wide
func parseBan(af *Agentfile, rest string, lineNo int) error {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return fmt.Errorf("line %d: BAN requires an @org/name reference", lineNo)
	}
	ref := fields[0]
	if !strings.HasPrefix(ref, "@") {
		return fmt.Errorf("line %d: BAN reference must start with @ (got %q)", lineNo, ref)
	}
	if strings.Contains(ref, ":") {
		return fmt.Errorf("line %d: BAN bans an agent by name, not a version; write @org/name (got %q)", lineNo, ref)
	}
	if !strings.Contains(ref[1:], "/") {
		return fmt.Errorf("line %d: BAN reference must be @org/name (got %q)", lineNo, ref)
	}

	var tools []string
	if len(fields) > 1 {
		if !strings.EqualFold(fields[1], "ONLY") {
			return fmt.Errorf("line %d: BAN expected ONLY after reference, got %q", lineNo, fields[1])
		}
		if len(fields) < 3 {
			return fmt.Errorf("line %d: BAN ONLY requires at least one tool name", lineNo)
		}
		tools = parseDenyList(strings.Join(fields[2:], " "))
		if len(tools) == 0 {
			return fmt.Errorf("line %d: BAN ONLY requires at least one non-empty tool name", lineNo)
		}
	}

	af.Ban = append(af.Ban, Ban{Ref: ref, Tools: tools})
	return nil
}

func parseBudget(af *Agentfile, rest string, lineNo int) error {
	if af.Budget != 0 {
		return fmt.Errorf("line %d: BUDGET declared twice", lineNo)
	}
	fields := strings.Fields(rest)
	if len(fields) != 1 {
		return fmt.Errorf("line %d: BUDGET takes a single USD amount like 5.00", lineNo)
	}
	micros, err := parseUSDMicros(fields[0])
	if err != nil {
		return fmt.Errorf("line %d: BUDGET %q is not a USD amount", lineNo, fields[0])
	}
	if micros <= 0 {
		return fmt.Errorf("line %d: BUDGET must be positive", lineNo)
	}
	af.Budget = micros
	return nil
}

// parseUSDMicros turns a USD amount like "5", "5.00", or "0.003" into
// integer micro-USD (millionths of a dollar) so budgets accumulate without
// float drift. More than six fractional digits is finer than we track and
// is rejected.
func parseUSDMicros(s string) (int64, error) {
	whole, frac, hasFrac := strings.Cut(s, ".")
	var dollars int64
	if whole != "" {
		d, err := strconv.ParseInt(whole, 10, 64)
		if err != nil || d < 0 {
			return 0, errors.New("invalid USD amount")
		}
		dollars = d
	}
	micros := dollars * 1_000_000
	if hasFrac {
		if len(frac) > 6 {
			return 0, errors.New("USD amount has more than six decimal places")
		}
		for len(frac) < 6 {
			frac += "0"
		}
		f, err := strconv.ParseInt(frac, 10, 64)
		if err != nil || f < 0 {
			return 0, errors.New("invalid USD amount")
		}
		micros += f
	}
	return micros, nil
}

// parseResources records the advisory RESOURCES hint. It captures
// well-formed cpu/mem/pids values and rejects unknown keys; the operator,
// not these numbers, sets the enforced cap.
func parseResources(af *Agentfile, rest string, lineNo int) error {
	if af.Resources != nil {
		return fmt.Errorf("line %d: RESOURCES declared twice", lineNo)
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return fmt.Errorf("line %d: RESOURCES requires at least one of cpu=, mem=, pids=", lineNo)
	}
	res := &Resources{}
	for _, field := range fields {
		key, val, ok := strings.Cut(field, "=")
		if !ok || val == "" {
			return fmt.Errorf("line %d: RESOURCES expects key=value (got %q)", lineNo, field)
		}
		switch key {
		case "cpu":
			res.CPUs = val
		case "mem":
			res.Mem = val
		case "pids":
			n, err := strconv.Atoi(val)
			if err != nil || n <= 0 {
				return fmt.Errorf("line %d: RESOURCES pids must be a positive integer (got %q)", lineNo, val)
			}
			res.Pids = n
		default:
			return fmt.Errorf("line %d: RESOURCES unknown key %q (want cpu, mem, pids)", lineNo, key)
		}
	}
	af.Resources = res
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

func parseEgress(af *Agentfile, rest string, lineNo int) error {
	if af.Egress != "" {
		return fmt.Errorf("line %d: EGRESS declared twice", lineNo)
	}
	if rest == "" {
		return fmt.Errorf("line %d: EGRESS requires a policy", lineNo)
	}
	if rest != "deny-default" && !strings.HasPrefix(rest, "allow:") {
		return fmt.Errorf("line %d: EGRESS must be deny-default or allow:<domains> (got %q)", lineNo, rest)
	}
	af.Egress = rest
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
