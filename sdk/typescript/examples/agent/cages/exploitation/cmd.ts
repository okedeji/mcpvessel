import { spawn } from 'child_process';

// Spawn a CLI tool with a bounded timeout. Returns stdout, stderr,
// and exit code. Failure is reported, not thrown — the caller decides
// whether to file a vulnerability finding or a clean-probe discovery
// finding noting the tool errored.
//
// Per-stream output is capped at 1MB to bound memory if a tool goes
// runaway (nuclei against a huge target, sqlmap with a chatty backend).
export async function runCmd(
  cmd: string,
  args: string[],
  timeoutMs: number,
): Promise<{ stdout: string; stderr: string; code: number | null }> {
  return new Promise((resolve) => {
    const child = spawn(cmd, args, { stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = '';
    let stderr = '';
    let stdoutSize = 0;
    let stderrSize = 0;
    const MAX = 1 << 20;
    child.stdout.on('data', (d: Buffer) => {
      if (stdoutSize < MAX) {
        stdout += d.toString();
        stdoutSize += d.length;
      }
    });
    child.stderr.on('data', (d: Buffer) => {
      if (stderrSize < MAX) {
        stderr += d.toString();
        stderrSize += d.length;
      }
    });
    const timer = setTimeout(() => child.kill('SIGKILL'), timeoutMs);
    child.on('close', (code) => {
      clearTimeout(timer);
      resolve({ stdout, stderr, code });
    });
    child.on('error', (err) => {
      clearTimeout(timer);
      resolve({ stdout, stderr: stderr + '\n' + String(err), code: -1 });
    });
  });
}
