import * as net from 'net';
import type { AgentFinding } from '../types/agent';

const DEFAULT_SOCKET = '/var/run/agentcage/findings.sock';

export class FindingsWriter {
  private socket: net.Socket | null = null;
  private socketPath: string;

  constructor(socketPath: string = DEFAULT_SOCKET) {
    this.socketPath = socketPath;
  }

  private async ensureConnection(): Promise<net.Socket> {
    if (this.socket && !this.socket.destroyed) {
      return this.socket;
    }
    return new Promise((resolve, reject) => {
      const sock = net.createConnection(this.socketPath, () => {
        this.socket = sock;
        resolve(sock);
      });
      sock.on('error', (err) => {
        this.socket = null;
        reject(new Error(`findings socket connection failed: ${err.message}`));
      });
    });
  }

  async submit(finding: AgentFinding): Promise<void> {
    // Vulnerability findings must include reproduction steps so the
    // validator cage can confirm them independently. Surface/info
    // findings (e.g. discovery results) don't need a proof.
    if (finding.severity !== 'info' && !finding.validationProof?.reproductionSteps) {
      throw new Error(`finding ${finding.id}: validationProof.reproductionSteps is required for severity=${finding.severity}`);
    }
    const sock = await this.ensureConnection();

    // Default status to candidate if not set.
    const payload = {
      ...finding,
      status: finding.status ?? 'candidate',
      evidence: finding.evidence
        ? {
            request: finding.evidence.request?.toString('base64'),
            response: finding.evidence.response?.toString('base64'),
            screenshot: finding.evidence.screenshot?.toString('base64'),
            poc: finding.evidence.poc,
            metadata: finding.evidence.metadata,
          }
        : undefined,
    };

    const line = JSON.stringify(payload) + '\n';

    return new Promise((resolve, reject) => {
      const ok = sock.write(line, 'utf-8', (err) => {
        if (err) reject(new Error(`writing finding: ${err.message}`));
        else resolve();
      });
      if (!ok) {
        sock.once('drain', () => resolve());
      }
    });
  }

  close(): void {
    this.socket?.destroy();
    this.socket = null;
  }
}
