#!/usr/bin/env bash
# Flash firmware onto the earrings attached to the remote flashing machine.
#
#   tools/flash.sh [-h host] <target> <firmware.hex> [more.hex ...]
#
# <target> is which earring to write:
#
#   left, right   the logical names below
#   <port>        a raw USB port path on the host (e.g. 1-1.1), stable as
#                 long as the adapters stay in the same physical sockets
#   all           every earring in PORTS, in order
#
# The flashing host (default: pi, root) has one ST-Link per earring. The
# probes are clones reporting the same garbage serial, so `adapter serial`
# cannot tell them apart; `adapter usb location <busid>` selects by USB
# port path, which is what this script uses.
#
# Examples:
#   tools/flash.sh left debug/earring-ring-rgb36v2/firmware.hex
#   tools/flash.sh all earring-ring-rgb36v2/firmware.hex
#   FLASH_HOST=otherpi tools/flash.sh right f.hex
set -euo pipefail

HOST="${FLASH_HOST:-pi}"
SELF=$(basename "$0")

# Earring name -> USB port path of its ST-Link on $HOST. Re-map these if
# the cables move; `ssh $HOST 'for d in /sys/bus/usb/devices/1-1.*; do
# [ -f $d/idVendor ] || continue; echo $d $(cat $d/idProduct); done'`
# shows what is where (0483:3748 = ST-Link).
declare -A PORTS=( [left]=1-1.1 [right]=1-1.2 )

usage() {
	sed -n '3,16p' "$0" | cut -c 3-
	echo "known targets: ${!PORTS[*]} (or a raw port path, or all)" >&2
	exit 2
}

[ $# -ge 2 ] || usage
TARGET=$1
shift

TARGETS=()
case $TARGET in
	all)   TARGETS=("${PORTS[@]}") ;;
	*)     TARGETS=("${PORTS[$TARGET]:-$TARGET}") ;;
esac
[ ${#TARGETS[@]} -gt 0 ] || usage

REMOTE=/tmp/flash-$$.hex

# Write $REMOTE through the adapter on USB port $port. $1 is extra openocd
# options, interpolated into the remote command line so the remote shell sees
# them as separate quoted arguments.
write() {
	ssh "$HOST" "openocd -c 'adapter usb location $port' \
			-f interface/stlink.cfg -f target/stm32l0.cfg $1 \
			-c 'program $REMOTE verify reset exit'"
}

# A healthy earring takes the plain form. This one only answers with SRST held
# and the link slowed to 100 kHz, which is a tired cell rather than wiring: the
# targets used to report 3.273/3.253 V and now read 3.254/3.248. Kept as a
# fallback instead of the default so a healthy device still flashes the fast way
# and a device that needs it says so in the log.
SRST="-c 'reset_config srst_only connect_assert_srst' -c 'adapter speed 100' \
	-c 'init' -c 'halt'"

for hex in "$@"; do
	[ -f "$hex" ] || { echo "$SELF: no such file: $hex" >&2; exit 1; }
	echo "== $hex -> $HOST (${TARGETS[*]})"
	scp -q "$hex" "$HOST:$REMOTE"
	status=0
	for port in "${TARGETS[@]}"; do
		ssh "$HOST" 'pkill -x openocd' 2>/dev/null && sleep 1
		if write ""; then
			echo "== $port: OK"
		elif write "$SRST"; then
			echo "== $port: OK (needed SRST at 100 kHz: this cell is tired)" >&2
		else
			echo "== $port: FAILED" >&2
			status=1
		fi
	done
done
ssh "$HOST" "rm -f $REMOTE" 2>/dev/null || true
exit $status
