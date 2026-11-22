#!/usr/bin/env bash
# Run a command up to TRIES times, pausing longer after each failed try, and
# exit with the last try's code when every try fails (#372).
#
# CI's "Check, build" job fetches origin/main over the network for its script
# test coverage step; one dropped fetch reds the required gate on a commit whose
# gates pass, and costs the PR a whole extra CI round. The pause after try n is
# n * RETRY_DELAY seconds (default 5), with none after the last. Must stay
# bash-3.2-safe (macOS stock).
#
# Usage:
#   retry.sh TRIES COMMAND [ARG...]
set -euo pipefail

usage() {
	echo "usage: retry.sh TRIES COMMAND [ARG...]  (TRIES a whole number of at least 1)" >&2
	exit 64
}

[ $# -ge 2 ] || usage
tries="$1"
shift
case "$tries" in
'' | *[!0-9]*) usage ;;
esac
[ "$tries" -ge 1 ] || usage
delay="${RETRY_DELAY:-5}"

try=1
while :; do
	rc=0
	"$@" || rc=$?
	[ "$rc" -ne 0 ] || exit 0
	if [ "$try" -ge "$tries" ]; then
		echo "retry.sh: $1 gave up after $tries tries (exit $rc)" >&2
		exit "$rc"
	fi
	echo "retry.sh: $1 try $try of $tries failed (exit $rc); retrying in $((try * delay))s" >&2
	sleep "$((try * delay))"
	try=$((try + 1))
done
