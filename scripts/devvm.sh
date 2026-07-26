#!/usr/bin/env bash
#
# devvm.sh drives an isolated mcpvessel runtime for live dev tests, fully
# separate from the operator's real ~/.mcpvessel: its own VESSEL_HOME, its own
# Lima VM, its own daemon socket. It never touches the real install or its VM.
#
# Speed: the dev VM is provisioned once and then kept on disk. 'down' stops it
# (fast to restart); 'up' reuses an already-provisioned VM instead of building a
# new one; only the first 'up' pays the containerd/buildkit install cost.
#
# Usage:
#   scripts/devvm.sh rebuild        # rebuild host + in-VM binaries with local code
#   scripts/devvm.sh up             # provision (first time) or start the dev VM + daemon
#   scripts/devvm.sh <cmd> [args]   # run any mcpvessel command against the dev runtime
#                                   #   e.g. scripts/devvm.sh build ./dir -t @dev/x:0.1
#   scripts/devvm.sh down           # stop the daemon and the VM, keep the disk (fast next 'up')
#   scripts/devvm.sh clean          # stop and DELETE the dev VM and VESSEL_HOME entirely
#   scripts/devvm.sh status         # show the dev VM and daemon state
#
# Reserved subcommands are rebuild/up/down/clean/status/help; every other first
# arg (build, run, serve, replay, ...) passes straight through to mcpvessel.
#
# Override the dev home with MCPVESSEL_DEV_HOME. It defaults OUTSIDE the repo so
# the 60GB VM disk never lands in the tree.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export VESSEL_HOME="${MCPVESSEL_DEV_HOME:-$HOME/.mcpvessel-dev}"
BIN="$REPO/bin/mcpvessel"
LIMACTL="$REPO/bin/lima/bin/limactl"
LIMA_DATA="$VESSEL_HOME/lima/data"

# A smaller VM than the default 8GiB so it coexists with the operator's real VM.
DEV_MEM_GIB="${MCPVESSEL_DEV_MEM_GIB:-4}"
DEV_CPUS="${MCPVESSEL_DEV_CPUS:-2}"

require_bin() {
	if [ ! -x "$BIN" ]; then
		echo "dev binary $BIN missing; run 'scripts/devvm.sh build' first" >&2
		exit 1
	fi
}

case "${1:-}" in
rebuild)
	make -C "$REPO" build build-linux
	;;

up)
	require_bin
	mkdir -p "$VESSEL_HOME"
	# Size the dev VM small before the first init reads it.
	"$BIN" config machine set --memory-gib "$DEV_MEM_GIB" --cpus "$DEV_CPUS" >/dev/null 2>&1 || true
	echo "Bringing up the isolated dev runtime at VESSEL_HOME=$VESSEL_HOME"
	echo "(first time provisions a ${DEV_MEM_GIB}GiB VM; later runs reuse it)"
	"$BIN" init
	echo "Dev runtime ready. Run commands with: scripts/devvm.sh <cmd>"
	;;

down)
	require_bin
	"$BIN" daemon stop >/dev/null 2>&1 || true
	if [ -x "$LIMACTL" ] && [ -d "$LIMA_DATA" ]; then
		LIMA_HOME="$LIMA_DATA" "$LIMACTL" stop mcpvessel 2>/dev/null || true
	fi
	echo "Dev daemon and VM stopped (disk kept; next 'up' is fast)."
	;;

clean)
	require_bin
	"$BIN" daemon stop >/dev/null 2>&1 || true
	if [ -x "$LIMACTL" ] && [ -d "$LIMA_DATA" ]; then
		LIMA_HOME="$LIMA_DATA" "$LIMACTL" stop -f mcpvessel 2>/dev/null || true
		LIMA_HOME="$LIMA_DATA" "$LIMACTL" delete -f mcpvessel 2>/dev/null || true
	fi
	# The dev home is a plain dir; lima/data holds the VM. rm removes the symlink
	# entry if one exists, never a real target, but this harness never symlinks.
	rm -rf "$VESSEL_HOME"
	echo "Dev VM and $VESSEL_HOME removed."
	;;

status)
	echo "VESSEL_HOME=$VESSEL_HOME"
	if [ -x "$LIMACTL" ] && [ -d "$LIMA_DATA" ]; then
		LIMA_HOME="$LIMA_DATA" "$LIMACTL" list 2>/dev/null || echo "(no VM yet)"
	else
		echo "(no VM yet)"
	fi
	require_bin
	"$BIN" ps -a 2>/dev/null || echo "(daemon not running)"
	;;

"" | -h | --help | help)
	sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	;;

*)
	require_bin
	exec "$BIN" "$@"
	;;
esac
