#!/usr/bin/env bash
#
# dev-claude.sh launches a Claude Code session wired to the dev mcpvessel build
# instead of the installed brew one, WITHOUT cutting a release or `brew upgrade`.
#
# The trick: the skill and hooks call the bare name `mcpvessel` (PATH-resolved),
# and every command honours $VESSEL_HOME. So we start Claude from a shell where
#   - the repo's bin/ is first on PATH  -> bare `mcpvessel` = the dev binary
#   - VESSEL_HOME points at the dev home -> it drives the dev daemon + VM
# Claude Code passes this environment to the skill's commands and to both hooks,
# so the whole agent-driven flow runs against dev. Close the session and your
# normal shell still resolves `mcpvessel` to the brew prod binary as before.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export VESSEL_HOME="${MCPVESSEL_DEV_HOME:-$HOME/.mcpvessel-dev}"
export PATH="$REPO/bin:$PATH"
echo "dev session: mcpvessel -> $(command -v mcpvessel)  ($(mcpvessel --version))"
echo "             VESSEL_HOME=$VESSEL_HOME"
exec claude "$@"
