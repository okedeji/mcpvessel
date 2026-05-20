# Known issues

## Secrets in Terraform user data

**Status:** open
**Severity:** low (dev), high (prod)
**Affects:** `infra/terraform/modules/aws/agentcage/`

The Terraform module passes secrets to the instance via EC2 user data. This has two security gaps:

1. **User data is readable via the AWS API.** Any IAM principal with `ec2:DescribeInstanceAttribute` can retrieve the user data, which contains the plaintext secrets.
2. **Terraform state contains secrets in plaintext.** Even with `sensitive = true`, the values are stored in `terraform.tfstate`. Local state files or unencrypted S3 backends expose them.

**Acceptable for:** dev and test environments where the instance is short-lived and the AWS account is single-user.

**Not acceptable for:** production, shared accounts, or long-lived instances.

**Fix:** Use AWS Secrets Manager or SSM Parameter Store. The instance IAM role fetches secrets at boot instead of receiving them through user data. This removes secrets from both the Terraform state and the instance metadata.

## SDK distributed via GitHub release tarball, not npm

**Status:** open (workaround in place)
**Severity:** low
**Affects:** `sdk/typescript/`, `internal/cagefile/install.go`, `cmd/agentcage/cmd_sdk.go`

The `@agentcage/sdk` TypeScript package is not published to the npm registry. npm publish requires an OTP (one-time password) that could not be configured during initial development.

**Workaround:** The SDK is distributed as an npm-compatible tarball (`agentcage-sdk-X.Y.Z.tgz`) attached to each GitHub release. The orchestrator downloads it during `agentcage init` to `~/.agentcage/bin/agentcage-sdk.tgz`. During `agentcage pack`, the dependency resolver rewrites `@agentcage/sdk` in the agent's `package.json` to a `file://` path pointing at the cached tarball. This makes `npm install` resolve the SDK offline without a registry.

**User-facing behavior:**
- `agentcage sdk install` installs the SDK into the current project from the local cache.
- Agents declare `@agentcage/sdk` as a normal dependency. It resolves at pack time, not install time.
- No npm authentication or registry access is needed.

**Fix:** Publish `@agentcage/sdk` to npm (or a private registry) and remove the `file://` rewriting in `cagefile/install.go`. This would let agents install the SDK via standard `npm install` without the orchestrator.

## macOS not supported

**Status:** resolved (removed)
**Details:** see [docs/macos-removal.md](macos-removal.md)

agentcage requires Linux with `/dev/kvm`. The macOS support layer (Apple Virtualization.framework with nested KVM) was removed because Apple VZ does not expose VHE to the guest CPU, preventing Firecracker guests from booting. CLI commands (`run`, `logs`, `findings`) work from macOS against a remote Linux orchestrator.

## Screenshots stored inline in Postgres JSONB

**Status:** open
**Severity:** low (today), medium (at scale)
**Affects:** `internal/findings/types.go`, `internal/findings/pgstore.go`, `internal/findings/validate.go`

Finding screenshots are stored as `bytes` inside the `evidence` JSONB column on the `findings` table. JSON-encoded byte arrays inflate ~33% via base64, screenshots compete with row-size limits, and TOAST handles only the long-tail compression. The `SanitizeFinding` step caps each screenshot at 5MB (`DefaultMaxScreenshotSize`) precisely because there's no object storage option.

**Acceptable for:** assessments producing tens of findings with small PNGs (typical SPA login pages, single-component screenshots).

**Not acceptable for:** assessments producing hundreds of findings with full-page captures, or any workflow that needs to retain the visual evidence trail long-term. The 5MB cap drops oversized screenshots silently (with a note in the description) — agent authors should compress before submitting.

**Fix:** introduce an object store (S3 / MinIO / GCS) for evidence blobs. The `findings` row stores a reference (`screenshot_url string`) instead of bytes. `GetFinding` resolves the URL on read, `ListFindings` keeps the existing `has_screenshot` boolean so the list view stays cheap. Migration path: add the column, dual-write for one release, then drop the inline bytes.

## Non-descriptive exploitation tool names degrade silently

**Status:** open (convention-only enforcement)
**Severity:** low
**Affects:** `internal/cagefile/parse.go`, `internal/assessment/planner.go`

Trailing tokens on `EXPLOITATION <tool ...>` are opaque free-text by design — agentcage does not validate them against any taxonomy. If an agent author writes uninformative tool names (`EXPLOITATION thing1 asdf`), the orchestrator LLM cannot reason about them, produces poor exploitation plans (or sets `done=true` early), and the agent receives actions whose `vuln_class` does not match its dispatcher, returning empty findings and wasting cages.

**Mitigated by:** convention. Agent authors should pick descriptive tool names (e.g. `sqli`, `xss-mutator`, `idor-fuzzer`) that reflect what each tool tests for. Cost falls on the agent author through their own empty reports — self-correcting feedback loop.

**Fix:** add an optional `TOOL <name> "description"` Cagefile directive. The orchestrator would pass descriptions alongside tool names in `CoordinatorState`, giving the LLM real context for tools whose names are not obvious. Additive change; deferred until real agents in the wild show what shape descriptions take.

## Cage rootfs uses Alpine Linux (musl libc), not glibc

**Status:** open (intentional design, documented constraint)
**Severity:** low (most use cases), medium (ML-heavy agents, glibc-only commercial tools)
**Affects:** `scripts/build-cage-rootfs.sh`, agent authors

The cage rootfs is Alpine Linux, which uses musl libc instead of glibc. Chosen for size (~5MB base vs ~30MB for Debian-slim), faster apk installs, smaller attack surface, and container-native maturity. Agentcage's bounded-lifetime cage model is a good fit for Alpine's strengths.

Agent authors should know:

- **glibc-only binaries do not run.** Affects npm packages with native postinstalls (full `puppeteer` bundles a glibc Chromium that fails on Alpine — use `puppeteer-core` + `DEPS chromium` instead), some Go binaries with CGO enabled, Burp Suite Pro CLI, Frida, and other proprietary tools.
- **Python wheels: musllinux only.** Many ML libraries (PyTorch, sentence-transformers, NumPy with native ops) ship manylinux (glibc) wheels but not musllinux. Pip falls back to building from source — agent authors need `apk add python3-dev gcc musl-dev build-base` in their Cagefile, and pack time grows from seconds to minutes.
- **Smaller default stack size** (128KB vs 8MB). Deeply-recursive parsers and regex engines occasionally blow the stack on Alpine.
- **DNS resolver behavior** differs from glibc. The cage rootfs already applies `options single-request timeout:5 attempts:3` in resolv.conf for Go's pure resolver.

**Acceptable for:** typical web pentest agents using HTTP probing, the pre-installed CLI tools (sqlmap, nuclei, ffuf, chromium), and standard node/python/go runtime deps.

**Not acceptable for:** ML-heavy agents (slow installs, broken wheels), agents needing glibc-only commercial tools, agents that depend on npm packages with native postinstall scripts.

**Fix:** add opt-in rootfs variants. New Cagefile directive `BASE debian` selects a glibc rootfs; default `BASE alpine` stays the lean option. Orchestrator builds both ext4 images at release time; cage assembler picks per-cage based on the manifest. Cage-internal binaries already build with `CGO_ENABLED=0` and run on either base.

**Trigger conditions for shipping the debian variant:**
- Two or more external agent authors hit musl issues within a month
- A specific glibc-only tool integration is requested (Burp Suite Pro CLI is the most common in pentest-as-a-service)
- We add agentcage features that need PyTorch / transformers
