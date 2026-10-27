#!/bin/bash
# BENCH (do not commit): flash a given LED-noise level and capture N runs.
#   ledsweep.sh LEVEL RUNS OUTPREFIX [WAV] [SECONDS]
# Sets const benchLedLevel=LEVEL in sync.go, rebuilds, flashes, then runs
# tools/capture2.py RUNS times against WAV. OUTPREFIX gets _r1.csv, _r2.csv ...
set -e
LVL=$1; RUNS=$2; OUT=$3
WAV=${4:-/tmp/psync/h900.wav}
SEC=${5:-55}
cd /home/max/Documents/led-earrings/debug/earring-ring-rgb36v2

python3 - "$LVL" <<'PY'
import sys, re
s = open('sync.go').read()
s2 = re.sub(r'const benchLedLevel = \d+', 'const benchLedLevel = ' + sys.argv[1], s, count=1)
assert s2 != s or ('const benchLedLevel = ' + sys.argv[1]) in s, "knob not found/changed"
open('sync.go', 'w').write(s2)
PY

pkill -f openocd 2>/dev/null || true
sleep 0.5
tinygo build -o firmware.elf -target=./stm32l0x1.json -opt=2 -panic=trap . 2>&1 | tail -1
tinygo build -o firmware.hex -target=./stm32l0x1.json -opt=2 -panic=trap . >/dev/null 2>&1
echo -n "flash LVL=$LVL: "
openocd -f interface/stlink.cfg -f target/stm32l0.cfg -c 'program firmware.hex verify reset exit' 2>&1 | grep -iE "verified|error" | head -1
for r in $(seq 1 "$RUNS"); do
  python3 tools/capture2.py --file "$WAV" --out "${OUT}_r${r}.csv" --seconds "$SEC" 2>&1 | tail -1
done
