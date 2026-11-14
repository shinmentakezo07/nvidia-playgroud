#!/usr/bin/env bash
# Restart the whole local stack: WARP exits -> rotating SOCKS5 front -> serve.
#
# Order matters: the exits must answer before the rotator is useful, and the
# rotator must listen before serve starts minting captcha tokens through it,
# otherwise the first pool fill reports errors against a dead proxy.
#
#   scripts/warp/restart.sh              # build, stop everything, start fresh
#   scripts/warp/restart.sh --keep-exits # keep running usque tunnels, restart the rest
#   scripts/warp/restart.sh --no-build   # reuse the existing binaries as-is
#   scripts/warp/restart.sh --stop       # only stop the stack
#
# Every start rebuilds nvidia-proxy and warpsocks from source first, so the
# running stack always matches the working tree.
#
# Overridable: WARPDIR, EXITS(range), BIND(rotator), ADDR(serve), PORTBASE.
set -euo pipefail

# Resolve symlinks first: installed as `un` in PATH, $0 is the link, not the script.
SELF="$(readlink -f "${BASH_SOURCE[0]:-$0}")"
ROOT="$(cd "$(dirname "$SELF")/../.." && pwd)"
WARPDIR="${WARPDIR:-/data/home/warp}"
SERVE_BIN="${SERVE_BIN:-$ROOT/nvidia-proxy}"
ROTATE_BIN="${ROTATE_BIN:-$WARPDIR/bin/warpsocks}"
USQUE_BIN="${USQUE_BIN:-$WARPDIR/bin/usque}"
BIND="${BIND:-127.0.0.1:1080}"
ADDR="${ADDR:-:8080}"
PORTBASE="${PORTBASE:-4000}"
NUM_EXITS="${NUM_EXITS:-12}"
POOL_MIN="${POOL_MIN:-$NUM_EXITS}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-45}"
BUILD="${BUILD:-yes}"

log() { printf 'restart: %s\n' "$*"; }
die() { printf 'restart: %s\n' "$*" >&2; exit 1; }

# Rebuild from source so a restart never runs a stale binary: editing cmd/serve
# or internal/warpsocks has no effect until the binary on disk is replaced.
# usque is a third-party tool, built once only if its binary is missing.
build() {
	if [ "$BUILD" != "yes" ]; then
		log "skipping build (--no-build)"
		return 0
	fi
	log "building serve -> $SERVE_BIN"
	(cd "$ROOT" && go build -o "$SERVE_BIN" ./cmd/serve) || die "serve build failed"
	log "building warpsocks -> $ROTATE_BIN"
	(cd "$ROOT" && go build -o "$ROTATE_BIN" ./cmd/warpsocks) || die "warpsocks build failed"
	if [ ! -x "$USQUE_BIN" ]; then
		log "usque binary missing; building -> $USQUE_BIN"
		(cd "$WARPDIR" && GOBIN="$(dirname "$USQUE_BIN")" go install github.com/Diniboy1123/usque@latest) \
			|| die "usque build failed; download a release instead"
	fi
}

# Stop in reverse dependency order. Exact process names only: pkill -f would
# also match this script's own command line and kill the shell running it.
stop_all() {
	local keep_exits="${1:-no}"
	log "stopping serve"
	pkill -x nvidia-proxy 2>/dev/null || true
	log "stopping rotator"
	pkill -x warpsocks 2>/dev/null || true
	if [ "$keep_exits" = "yes" ]; then
		log "keeping usque exits"
	else
		log "stopping usque exits"
		pkill -x usque 2>/dev/null || true
	fi
	# Give listeners a moment to release their ports before rebinding.
	for _ in $(seq 1 20); do
		pgrep -x nvidia-proxy >/dev/null || pgrep -x warpsocks >/dev/null || break
		sleep 0.5
	done
}

wait_port() {
	local host=$1 port=$2 name=$3
	for _ in $(seq 1 60); do
		if (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; then
			exec 3>&- 3<&- 2>/dev/null || true
			return 0
		fi
		sleep 0.5
	done
	die "$name never listened on $host:$port"
}

exits_list() {
	local out="" i
	for i in $(seq 1 "$NUM_EXITS"); do
		out="${out:+$out,}127.0.0.1:$((PORTBASE + i))"
	done
	printf '%s' "$out"
}

start_exits() {
	local i port cfg
	for i in $(seq 1 "$NUM_EXITS"); do
		port=$((PORTBASE + i))
		cfg="$WARPDIR/cfg$i.json"
		[ -f "$cfg" ] || die "missing $cfg; register a WARP identity first (usque register -a -c $cfg)"
		if pgrep -x usque >/dev/null && (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
			exec 3>&- 3<&- 2>/dev/null || true
			continue
		fi
		setsid nohup "$USQUE_BIN" socks -c "$cfg" -b 127.0.0.1 -p "$port" --always-reconnect \
			>>"$WARPDIR/socks$i.log" 2>&1 </dev/null &
	done
	log "started $NUM_EXITS exits on $PORTBASE+1..$((PORTBASE + NUM_EXITS))"
	# MASQUE handshakes take a few seconds; probe until the first ones answer.
	for i in 1 2 3; do
		wait_port 127.0.0.1 $((PORTBASE + i)) "exit $((PORTBASE + i))"
	done
}

start_rotate() {
	local port=${BIND##*:}
	setsid nohup "$ROTATE_BIN" -exits "$(exits_list)" -addr "$BIND" -v \
		>>"$WARPDIR/rotate.log" 2>&1 </dev/null &
	wait_port "${BIND%:*}" "$port" rotator
	log "rotator on $BIND over $NUM_EXITS exits"
}

start_serve() {
	# No pool flags: cmd/serve defaults are the measured ones (3 workers/12 tokens).
	setsid nohup "$SERVE_BIN" -auto -addr "$ADDR" -proxy "socks5://$BIND" \
		>>"$WARPDIR/serve.log" 2>&1 </dev/null &
	local port=${ADDR##*:}
	wait_port 127.0.0.1 "$port" serve
	for _ in $(seq 1 "$HEALTH_TIMEOUT"); do
		h=$(curl -sf -m 3 "http://127.0.0.1:$port/healthz" 2>/dev/null || true)
		# ok:true only means the process is up; require a filled pool, or the
		# first requests race an empty pool and 503.
		if printf '%s' "$h" | grep -q '"ok":true' &&
			[ "$(printf '%s' "$h" | grep -o '"ready":[0-9]*' | cut -d: -f2)" -ge "$POOL_MIN" ]; then
			log "serve healthy: $h"
			return 0
		fi
		sleep 1
	done
	die "serve started but /healthz never reported ok; see $WARPDIR/serve.log"
}

main() {
	KEEP_EXITS=no
	case "${1:-}" in
	--stop)
		stop_all no
		log "stopped"
		exit 0
		;;
	--no-build) BUILD=no ;;
	--keep-exits) KEEP_EXITS=yes ;;
	esac
	build
	[ -x "$SERVE_BIN" ] || die "serve binary missing: $SERVE_BIN"
	[ -x "$ROTATE_BIN" ] || die "rotator binary missing: $ROTATE_BIN"
	[ -x "$USQUE_BIN" ] || die "usque binary missing: $USQUE_BIN"
	stop_all "$KEEP_EXITS"
	start_exits
	start_rotate
	start_serve
	log "done"
}

main "$@"
