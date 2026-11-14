#!/usr/bin/env bash
#
# keepalive.sh - ping a web service so it never idles out.
#
# Render's free plan spins a web service down after ~15 minutes with no
# inbound traffic, and the next request pays a ~50s cold start. One HTTP hit
# every 10 minutes keeps it warm.
#
# /healthz is the right target: cmd/serve answers it in-process (it only
# reports the captcha pool counters) and never calls the upstream NVIDIA API,
# so pinging it is cheap and cannot get you rate-limited.
#
# Usage:
#   ./scripts/keepalive.sh https://my-app.onrender.com
#   KEEPALIVE_URL=https://my-app.onrender.com ./scripts/keepalive.sh
#   ./scripts/keepalive.sh --once https://my-app.onrender.com   # single ping
#
# Environment:
#   KEEPALIVE_URL       base URL of the service (required unless given as $1)
#   KEEPALIVE_PATH      path to ping (default: /healthz)
#   KEEPALIVE_INTERVAL  seconds between pings (default: 600 = 10 min, must stay
#                       below the 15 min idle window)
#   KEEPALIVE_TIMEOUT   per-request timeout in seconds (default: 90, because a
#                       wake-up from sleep can take most of a minute)
#   KEEPALIVE_LOG       append log lines to this file instead of stderr
#
# Keep it running on a small VPS / always-on box:
#   nohup ./scripts/keepalive.sh https://my-app.onrender.com >/dev/null 2>&1 &
#   # or wrap it in a systemd unit, tmux, or a screen session
set -euo pipefail

once=0
url="${KEEPALIVE_URL:-}"

while [ $# -gt 0 ]; do
	case "$1" in
	-1 | --once) once=1 ;;
	-h | --help) sed -n '2,32p' "$0"; exit 0 ;;
	-*) echo "keepalive: unknown option $1" >&2; exit 2 ;;
	*) url="$1" ;;
	esac
	shift
done

if [ -z "$url" ]; then
	echo "keepalive: service URL required (argument or KEEPALIVE_URL)" >&2
	exit 2
fi

path="${KEEPALIVE_PATH:-/healthz}"
interval="${KEEPALIVE_INTERVAL:-600}"
timeout="${KEEPALIVE_TIMEOUT:-90}"
logfile="${KEEPALIVE_LOG:-}"
# Accept either a base URL or a URL that already ends with the ping path.
case "$url" in
*"$path") target="$url" ;;
*) target="${url%/}$path" ;;
esac

logline() {
	local line
	line="$(date -u '+%Y-%m-%dT%H:%M:%SZ') keepalive $*"
	if [ -n "$logfile" ]; then
		printf '%s\n' "$line" >>"$logfile"
	else
		printf '%s\n' "$line" >&2
	fi
}

# ping_once returns 0 on HTTP 200 and 1 otherwise; it never exits the script.
ping_once() {
	local res code secs rc=0 err msg
	err="$(mktemp "${TMPDIR:-/tmp}/keepalive.XXXXXX")"
	if res="$(curl -sS --max-time "$timeout" -o /dev/null \
		-w '%{http_code} %{time_total}' "$target" 2>"$err")"; then
		code="${res%% *}"
		secs="${res##* }"
		if [ "$code" = 200 ]; then
			rm -f "$err"
			logline "ok" "HTTP 200 in ${secs}s $target"
			return 0
		fi
		msg="HTTP $code in ${secs}s"
	else
		rc=$?
		msg="curl exit $rc: $(tr '\n' ' ' <"$err" | sed 's/ *$//')"
	fi
	rm -f "$err"
	logline "fail" "$msg $target"
	return 1
}

if [ "$once" = 1 ]; then
	ping_once
	exit $?
fi

logline "start" "every ${interval}s -> $target"
trap 'logline "stop" "signal received"; exit 0' INT TERM

failures=0
while :; do
	started=$SECONDS
	if ping_once; then
		failures=0
	elif [ "$failures" -eq 0 ]; then
		# A service waking up can answer slowly once; retry short so a single
		# miss does not leave a 10 minute hole in the schedule.
		failures=1
		sleep 20
		if ping_once; then
			failures=0
		else
			failures=2
		fi
	else
		failures=$((failures + 1))
	fi
	if [ "$failures" -gt 0 ]; then
		logline "warn" "$failures consecutive failure(s)"
	fi
	elapsed=$((SECONDS - started))
	if [ "$elapsed" -lt "$interval" ]; then
		sleep $((interval - elapsed))
	fi
done
