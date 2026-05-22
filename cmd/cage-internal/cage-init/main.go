package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CageEnv mirrors cage.Env. The config injected by the rootfs
// assembler at /etc/agentcage/cage.json.
type CageEnv struct {
	CageID                     string            `json:"cage_id"`
	AssessmentID               string            `json:"assessment_id"`
	CustomerID                 string            `json:"customer_id,omitempty"`
	CageType                   string            `json:"cage_type"`
	Entrypoint                 string            `json:"entrypoint"`
	Objective                  string            `json:"objective,omitempty"`
	LLMEndpoint                string            `json:"llm_endpoint,omitempty"`
	LLMAPIKey                  string            `json:"llm_api_key,omitempty"`
	JudgeAPIKey                string            `json:"judge_api_key,omitempty"`
	NATSAddr                   string            `json:"nats_addr,omitempty"`
	ScopeHost                  string            `json:"scope_host"`
	ScopePorts                 []string          `json:"scope_ports,omitempty"`
	ScopePaths                 []string          `json:"scope_paths,omitempty"`
	TokenBudget                int64             `json:"token_budget,omitempty"`
	VulnClass                  string            `json:"vuln_class,omitempty"`
	ParentFindingID            string            `json:"parent_finding_id,omitempty"`
	HoldsEnabled               bool              `json:"holds_enabled,omitempty"`
	HoldTimeoutSec             int               `json:"hold_timeout_sec,omitempty"`
	TargetCredentials          json.RawMessage   `json:"target_credentials,omitempty"`
	JudgeEndpoint              string            `json:"judge_endpoint,omitempty"`
	JudgeConfidence            float64           `json:"judge_confidence,omitempty"`
	JudgeTimeoutSec            int               `json:"judge_timeout_sec,omitempty"`
	RequireJudgeForAllOutbound bool              `json:"require_judge_for_all_outbound,omitempty"`
	ProofThreshold             float64           `json:"proof_threshold,omitempty"`
	IdentifyInRequests         bool              `json:"identify_in_requests,omitempty"`
	CustomEnv                  map[string]string `json:"custom_env,omitempty"`
}

// Paths configurable via environment for unisolated mode where
// cage-init runs as a regular process instead of PID 1 inside a VM.
var (
	configPath = envOr("AGENTCAGE_CAGE_CONFIG", "/etc/agentcage/cage.json")
	socketDir  = envOr("AGENTCAGE_SOCKET_DIR", "/var/run/agentcage")
	agentDir   = envOr("AGENTCAGE_AGENT_DIR", "/opt/agent")
	sidecarDir = envOr("AGENTCAGE_SIDECAR_DIR", "/usr/local/bin")
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	env, err := loadConfig()
	if err != nil {
		fatal("loading cage config: %v", err)
	}

	fmt.Printf("cage-init: cage=%s assessment=%s type=%s\n", env.CageID, env.AssessmentID, env.CageType)

	// Persistent log on the rootfs. Every writeLog() call writes here
	// with Sync(), so agent stdout/stderr survives VM death. The
	// orchestrator reads this via debugfs after the cage terminates.
	initCageLog()

	// Boot marker for quick checks without parsing the full log.
	bootLog, _ := os.OpenFile("/cage-boot.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	writeBootLog := func(msg string) {
		if bootLog != nil {
			_, _ = fmt.Fprintf(bootLog, "%d %s\n", time.Now().Unix(), msg)
			_ = bootLog.Sync()
		}
		writeLog(nil, "system", msg)
	}
	writeBootLog("cage-init started")

	// Ensure socket directory exists before starting sidecars.
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		fatal("creating socket directory: %v", err)
	}

	// 1. Start findings-sidecar (forwards findings to host via vsock)
	sidecar := startService("findings-sidecar",
		sidecarDir+"/findings-sidecar",
		"-socket", socketDir+"/findings.sock",
		"-vsock-port", "55",
		"-assessment-id", env.AssessmentID,
		"-cage-id", env.CageID,
	)

	writeBootLog("findings-sidecar started")

	// 2. Start directive-sidecar
	directiveSidecar := startService("directive-sidecar",
		sidecarDir+"/directive-sidecar",
		"-directive-file", socketDir+"/directives.json",
		"-hold-socket", socketDir+"/hold.sock",
		"-log-socket", socketDir+"/logs.sock",
	)
	writeBootLog("directive-sidecar started")

	// 3. Start payload-proxy
	var proxy *exec.Cmd
	if env.ScopeHost != "" {
		proxyArgs := []string{
			"-listen", ":8080",
			"-target", fmt.Sprintf("https://%s", env.ScopeHost),
			"-cage-id", env.CageID,
			"-cage-type", env.CageType,
			"-assessment-id", env.AssessmentID,
		}
		if env.VulnClass != "" {
			proxyArgs = append(proxyArgs, "-vuln-class", env.VulnClass)
		}
		if env.LLMEndpoint != "" {
			proxyArgs = append(proxyArgs, "-llm-endpoint", env.LLMEndpoint)
		}
		if env.TokenBudget > 0 {
			proxyArgs = append(proxyArgs, "-token-budget", fmt.Sprintf("%d", env.TokenBudget))
		}
		if env.HoldsEnabled {
			proxyArgs = append(proxyArgs, "-holds-enabled")
			if env.HoldTimeoutSec > 0 {
				proxyArgs = append(proxyArgs, "-hold-timeout", fmt.Sprintf("%d", env.HoldTimeoutSec))
			}
		}
		if env.JudgeEndpoint != "" {
			proxyArgs = append(proxyArgs,
				"-judge-endpoint", env.JudgeEndpoint,
				"-judge-confidence", fmt.Sprintf("%.2f", env.JudgeConfidence),
			)
			if env.JudgeTimeoutSec > 0 {
				proxyArgs = append(proxyArgs, "-judge-timeout", fmt.Sprintf("%d", env.JudgeTimeoutSec))
			}
		}
		if env.RequireJudgeForAllOutbound {
			proxyArgs = append(proxyArgs, "-judge-all-outbound")
		}
		if env.Objective != "" {
			proxyArgs = append(proxyArgs, "-objective", env.Objective)
		}
		if env.IdentifyInRequests {
			proxyArgs = append(proxyArgs, "-identify-in-requests")
			if env.CustomerID != "" {
				proxyArgs = append(proxyArgs, "-customer-id", env.CustomerID)
			}
		}
		// CA for TLS interception. Generated per-cage during rootfs assembly.
		caCert := "/etc/agentcage/ca.pem"
		caKey := "/etc/agentcage/ca-key.pem"
		if _, err := os.Stat(caCert); err == nil {
			proxyArgs = append(proxyArgs, "-ca-cert", caCert, "-ca-key", caKey)
		}
		proxy = startService("payload-proxy", sidecarDir+"/payload-proxy", proxyArgs...)
	}

	// 3. Set up iptables to redirect outbound HTTP through the proxy.
	// Skipped in unisolated mode (iptables not available on macOS/non-root).
	if proxy != nil {
		setupIPTables(extractLLMPort(env.LLMEndpoint))
	}

	// 4. Export environment variables for the agent
	setEnv("AGENTCAGE_CAGE_ID", env.CageID)
	setEnv("AGENTCAGE_ASSESSMENT_ID", env.AssessmentID)
	setEnv("AGENTCAGE_CAGE_TYPE", env.CageType)
	setEnv("AGENTCAGE_SCOPE", env.ScopeHost)
	setEnv("AGENTCAGE_FINDINGS_SOCKET", socketDir+"/findings.sock")
	if env.LLMEndpoint != "" {
		setEnv("AGENTCAGE_LLM_ENDPOINT", env.LLMEndpoint)
	}
	if env.LLMAPIKey != "" {
		setEnv("AGENTCAGE_LLM_API_KEY", env.LLMAPIKey)
	}
	if env.JudgeAPIKey != "" {
		setEnv("AGENTCAGE_JUDGE_API_KEY", env.JudgeAPIKey)
	}
	if env.TokenBudget > 0 {
		setEnv("AGENTCAGE_TOKEN_BUDGET", fmt.Sprintf("%d", env.TokenBudget))
	}
	if env.Objective != "" {
		setEnv("AGENTCAGE_OBJECTIVE", env.Objective)
	}
	if len(env.TargetCredentials) > 0 {
		setEnv("AGENTCAGE_TARGET_CREDENTIALS", string(env.TargetCredentials))
	}
	if env.ProofThreshold > 0 {
		setEnv("AGENTCAGE_PROOF_THRESHOLD", fmt.Sprintf("%.2f", env.ProofThreshold))
	}
	if env.JudgeEndpoint != "" {
		setEnv("AGENTCAGE_JUDGE_AVAILABLE", "true")
	}
	if env.VulnClass != "" {
		setEnv("AGENTCAGE_VULN_CLASS", env.VulnClass)
	}
	if env.ParentFindingID != "" {
		setEnv("AGENTCAGE_PARENT_FINDING_ID", env.ParentFindingID)
	}
	if len(env.ScopePaths) > 0 {
		setEnv("AGENTCAGE_SCOPE_PATHS", strings.Join(env.ScopePaths, ","))
	}
	if len(env.ScopePorts) > 0 {
		setEnv("AGENTCAGE_SCOPE_PORTS", strings.Join(env.ScopePorts, ","))
	}
	setEnv("AGENTCAGE_DIRECTIVES_FILE", socketDir+"/directives.json")
	setEnv("AGENTCAGE_HOLD_SOCKET", socketDir+"/hold.sock")
	setEnv("AGENTCAGE_LOG_SOCKET", socketDir+"/logs.sock")
	for k, v := range env.CustomEnv {
		setEnv(k, v)
	}

	writeBootLog("env exported, connecting log socket")

	// 6. Connect to the log socket so agent output reaches the orchestrator.
	logSocket := socketDir + "/logs.sock"
	logConn := connectLogSocket(logSocket)

	// 7. Wait for log pipe to be established. The directive-sidecar
	// writes a readiness file after connecting to the host via vsock.
	// Without this, agent output is lost if it prints before the pipe is up.
	waitForLogReady(socketDir + "/logs.ready")

	// Drain any sidecar output captured before the log socket was up.
	// New writes pass straight through. Silent sidecar failures used to
	// cost an hour of debugging; now they show up in cage logs.
	if logConn != nil {
		attachSidecarLogs(logConn)
	}

	writeBootLog("log ready, starting agent")
	writeLog(logConn, "system", fmt.Sprintf("cage=%s type=%s target=%s", env.CageID, env.CageType, env.ScopeHost))
	writeLog(logConn, "system", fmt.Sprintf("starting agent: %s", env.Entrypoint))

	// 8. Run the agent entrypoint.
	fmt.Printf("cage-init: exec agent: %s\n", env.Entrypoint)

	parts := strings.Fields(env.Entrypoint)
	agentBin, err := exec.LookPath(parts[0])
	if err != nil {
		agentBin = agentDir + "/" + parts[0]
		if _, statErr := os.Stat(agentBin); statErr != nil {
			fatal("agent binary not found: %s (checked PATH and %s)", parts[0], agentDir)
		}
	}

	agentCmd := exec.Command(agentBin, parts[1:]...)
	agentCmd.Dir = agentDir

	// Pipe agent stdout/stderr through the log socket with source tagging.
	if logConn != nil {
		agentCmd.Stdout = newLogWriter(logConn, "agent")
		agentCmd.Stderr = newLogWriter(logConn, "agent")
	} else {
		agentCmd.Stdout = os.Stdout
		agentCmd.Stderr = os.Stderr
	}

	if err := agentCmd.Run(); err != nil {
		writeLog(logConn, "system", fmt.Sprintf("agent exited with error: %v", err))
		fmt.Printf("cage-init: agent exited with error: %v\n", err)
		writeResult(1, err.Error())
		drainAndExit(logConn, []*exec.Cmd{sidecar, directiveSidecar, proxy}, 1)
	}

	writeLog(logConn, "system", "agent completed successfully")
	fmt.Println("cage-init: agent completed successfully")
	writeResult(0, "")
	drainAndExit(logConn, []*exec.Cmd{sidecar, directiveSidecar, proxy}, 0)
}

func loadConfig() (*CageEnv, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", configPath, err)
	}
	var env CageEnv
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &env, nil
}

// pendingSidecarLogs collects deferred log writers for every sidecar so
// we can attach them to the log socket once it connects. Sidecars start
// before the log socket is up; their output would otherwise be lost.
var pendingSidecarLogs []*deferredLogWriter

func startService(name string, bin string, args ...string) *exec.Cmd {
	out := newDeferredLogWriter(name)
	pendingSidecarLogs = append(pendingSidecarLogs, out)

	cmd := exec.Command(bin, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		fmt.Printf("cage-init: warning: failed to start %s: %v\n", name, err)
		return nil
	}
	fmt.Printf("cage-init: started %s (pid=%d)\n", name, cmd.Process.Pid)

	// Brief delay then check if the process crashed on startup.
	time.Sleep(200 * time.Millisecond)
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		fmt.Printf("cage-init: warning: %s exited immediately (code=%d)\n", name, cmd.ProcessState.ExitCode())
		return nil
	}

	return cmd
}

// attachSidecarLogs flushes any buffered sidecar output through logConn
// and routes subsequent writes there too. Call once logConn is open.
func attachSidecarLogs(logConn net.Conn) {
	for _, w := range pendingSidecarLogs {
		w.attach(logConn)
	}
}

// extractLLMPort returns the TCP port of the configured LLM endpoint,
// or "" if the endpoint is empty or unparseable. Used to add an iptables
// redirect so LLM calls go through the proxy and get metered.
func extractLLMPort(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

func setupIPTables(llmPort string) {
	// Redirect all outbound TCP 80/443 through the payload proxy on :8080.
	// The proxy sets SO_MARK=1 on its own outbound sockets so its upstream
	// connections skip the redirect and reach the real target. Without this,
	// the proxy's forwarded requests loop back through port 8080.
	rules := [][]string{
		{"iptables", "-t", "nat", "-A", "OUTPUT", "-m", "mark", "--mark", "1", "-j", "RETURN"},
		{"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-port", "8080"},
		{"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "tcp", "--dport", "443", "-j", "REDIRECT", "--to-port", "8080"},
	}
	// Without this redirect, LLM calls to a non-standard port bypass the
	// proxy entirely and tokens never get metered.
	if llmPort != "" && llmPort != "80" && llmPort != "443" {
		rules = append(rules,
			[]string{"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "tcp", "--dport", llmPort, "-j", "REDIRECT", "--to-port", "8080"},
		)
	}
	for _, args := range rules {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("cage-init: warning: iptables rule failed: %v\n%s\n", err, out)
		}
	}
	fmt.Printf("cage-init: iptables redirect rules applied (llm_port=%s)\n", llmPort)
}

func setEnv(key, value string) {
	os.Setenv(key, value) //nolint:errcheck
}

// writeResult persists the agent's exit code to the rootfs so the
// orchestrator can read it via debugfs after the VM dies and surface
// the failure in the cage and assessment status.
func writeResult(exitCode int, errMsg string) {
	data, _ := json.Marshal(map[string]any{
		"exit_code": exitCode,
		"error":     errMsg,
		"ts":        time.Now().Unix(),
	})
	if err := os.WriteFile("/var/log/cage-result.json", data, 0644); err != nil {
		return
	}
	if f, err := os.OpenFile("/var/log/cage-result.json", os.O_RDONLY, 0); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
}

func drainAndExit(logConn net.Conn, procs []*exec.Cmd, code int) {
	// Close the log socket so directive-sidecar knows no more lines
	// are coming and can flush its buffer through vsock.
	if logConn != nil {
		_ = logConn.Close()
	}
	// Give directive-sidecar time to forward buffered lines to the
	// host. The rootfs log is the authoritative source regardless,
	// but this improves the live-streaming experience.
	time.Sleep(3 * time.Second)
	for _, cmd := range procs {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	if cageLog != nil {
		_ = cageLog.Close()
	}
	os.Exit(code)
}

func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "cage-init: fatal: %s\n", msg)
	// Write to rootfs log so the orchestrator can recover the error.
	if cageLog != nil {
		line := fmt.Sprintf(`{"source":"system","msg":"fatal: %s","ts":%d}`, msg, time.Now().Unix())
		_, _ = cageLog.WriteString(line + "\n")
		_ = cageLog.Sync()
		_ = cageLog.Close()
	}
	os.Exit(1)
}

// waitForLogReady polls for the readiness file that the directive-sidecar
// writes after connecting to the host via vsock. The file confirms the
// log pipe is working end-to-end. Refuses to start the agent without it.
func waitForLogReady(path string) {
	for i := 0; i < 30; i++ {
		if _, err := os.Stat(path); err == nil {
			fmt.Println("cage-init: host log collector ready")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	// The rootfs log (/var/log/cage.log) captures every line
	// regardless, so the agent can run without live streaming.
	fmt.Fprintf(os.Stderr, "cage-init: warning: host log collector not ready after 15s, starting agent without live streaming\n")
	writeLog(nil, "system", "host log collector not ready after 15s, continuing without live streaming")
}

// connectLogSocket connects to the directive-sidecar's log socket.
// Returns nil if unavailable (logs fall back to stdout).
func connectLogSocket(path string) net.Conn {
	for attempt := 0; attempt < 10; attempt++ {
		conn, err := net.Dial("unix", path)
		if err == nil {
			return conn
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "cage-init: warning: log socket unavailable, using stdout\n")
	return nil
}

// cageLog persists every log line to the rootfs so the orchestrator
// can recover them via debugfs after the VM dies.
var cageLog *os.File

func initCageLog() {
	_ = os.MkdirAll("/var/log", 0755)
	f, err := os.OpenFile("/var/log/cage.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return
	}
	cageLog = f
}

// writeLog sends a tagged JSON log line to the vsock-forwarded socket
// (best effort) and to the persistent rootfs log (reliable).
func writeLog(conn net.Conn, source, msg string) {
	line := fmt.Sprintf(`{"source":%q,"msg":%q,"ts":%d}`, source, msg, time.Now().Unix())
	if cageLog != nil {
		_, _ = cageLog.WriteString(line + "\n")
		_ = cageLog.Sync()
	}
	if conn != nil {
		_, _ = conn.Write([]byte(line + "\n"))
	} else {
		fmt.Printf("[%s] %s\n", source, msg)
	}
}

// logWriter implements io.Writer and tags each line with a source.
type logWriter struct {
	conn   net.Conn
	source string
	buf    []byte
}

func newLogWriter(conn net.Conn, source string) *logWriter {
	return &logWriter{conn: conn, source: source}
}

// deferredLogWriter buffers writes in memory until attach() is called
// with a real log connection. Sidecars start before the log socket is
// open, so without this their stderr would be silently dropped.
type deferredLogWriter struct {
	mu      sync.Mutex
	source  string
	pending []byte
	inner   *logWriter
}

func newDeferredLogWriter(source string) *deferredLogWriter {
	return &deferredLogWriter{source: source}
}

func (w *deferredLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inner != nil {
		return w.inner.Write(p)
	}
	// Cap the buffer so a sidecar logging in a hot loop before attach
	// can't exhaust memory.
	const maxBuffered = 64 * 1024
	if len(w.pending)+len(p) > maxBuffered {
		drop := len(w.pending) + len(p) - maxBuffered
		if drop > len(w.pending) {
			drop = len(w.pending)
		}
		w.pending = w.pending[drop:]
	}
	w.pending = append(w.pending, p...)
	return len(p), nil
}

func (w *deferredLogWriter) attach(conn net.Conn) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inner = newLogWriter(conn, w.source)
	if len(w.pending) > 0 {
		_, _ = w.inner.Write(w.pending)
		w.pending = nil
	}
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := indexOf(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(w.buf[:idx])
		w.buf = w.buf[idx+1:]
		if line == "" {
			continue
		}
		writeLog(w.conn, w.source, line)
	}
	return len(p), nil
}

func indexOf(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}
