#!/usr/bin/env bash
# Set a tunable in syncdsp/consts.go, run the host tests, rebuild and flash.
#
# The tunables are const on this branch (TinyGo folds unwritten vars into
# immediates, so live SWD pokes would hit dead memory), which makes the
# reflash the parameter-sweep mechanism: booting fresh per run is also what
# keeps runs comparable from frame 0.
#
# Usage: tools/exp.sh DMin 3
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ $# -ne 2 ]]; then
	echo "usage: $0 <CONST_NAME> <value>" >&2
	exit 2
fi
name=$1
value=$2

# grep -E does not interpret \t, and the comment after the value must
# survive the rewrite - only the integer is replaced.
if ! grep -qE "^[[:space:]]*${name}[[:space:]]*=[[:space:]]*[0-9]+([[:space:]]|$)" syncdsp/consts.go; then
	echo "$0: ${name} not an integer tunable in syncdsp/consts.go" >&2
	exit 1
fi
sed -i -E "s/^([[:space:]]*${name}[[:space:]]*=[[:space:]]*)[0-9]+/\1${value}/" syncdsp/consts.go
if ! grep -qE "^[[:space:]]*${name}[[:space:]]*=[[:space:]]*${value}([[:space:]]|$)" syncdsp/consts.go; then
	echo "$0: rewrite of ${name} did not take" >&2
	exit 1
fi
echo "== ${name} = ${value}"

# Host tests first: a parameter that breaks bit-identical agreement between
# synthetic listeners should not reach the device at all.
go test -count=1 ./syncdsp/

tinygo build -o firmware.elf -target=./stm32l0x1.json -opt=2 -panic=trap -size=short .
tinygo build -o firmware.hex -target=./stm32l0x1.json -opt=2 -panic=trap .

# The flashing adapter is single-owner: stop the telemetry server first,
# restart it afterwards (spawn_server in telem.py also does this on demand).
pkill -f "openocd.*telnet_port" 2>/dev/null || true
sleep 0.5
openocd -f interface/stlink.cfg -f target/stm32l0.cfg \
	-c "program firmware.hex verify reset exit" 2>&1 |
	grep -E "Verified|Error|error|overflow"
