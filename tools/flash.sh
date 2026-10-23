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
# clone probes all report the same garbage serial and openocd's
# `adapter serial` cannot take a `<` redirect (its command parser has no
# shell), so there is no way to name a probe from the command line.
# Instead we unbind the *other* adapters from the USB driver, flash, and
# rebind them - openocd then has exactly one probe to choose from.
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

# Run $1 on the host with every ST-Link bound except the one on USB port $2.
flash_one() {
	local cmd=$1 port=$2 others=() p
	for p in "${PORTS[@]}"; do
		[ "$p" = "$port" ] || others+=("$p")
	done
	ssh "$HOST" bash -s <<EOF
set -e
cleanup() {
	for p in ${others[*]:-}; do
		[ -e /sys/bus/usb/devices/\$p ] && echo \$p > /sys/bus/usb/drivers/usb/bind
	done
}
trap cleanup EXIT
	pkill -x openocd 2>/dev/null || true
	for p in ${others[*]:-}; do
		[ -e /sys/bus/usb/devices/\$p ] && echo \$p > /sys/bus/usb/drivers/usb/unbind
	done
	sleep 1
	$cmd
EOF
}

for hex in "$@"; do
	[ -f "$hex" ] || { echo "$SELF: no such file: $hex" >&2; exit 1; }
	echo "== $hex -> $HOST (${TARGETS[*]})"
	scp -q "$hex" "$HOST:$REMOTE"
	status=0
	for port in "${TARGETS[@]}"; do
		# stlink.cfg picks the only bound probe; verify + reset after.
		if flash_one "openocd -f interface/stlink.cfg -f target/stm32l0.cfg \\
			-c 'program $REMOTE verify reset exit'" "$port"; then
			echo "== $port: OK"
		else
			echo "== $port: FAILED" >&2
			status=1
		fi
	done
done
ssh "$HOST" "rm -f $REMOTE" 2>/dev/null || true
exit $status
