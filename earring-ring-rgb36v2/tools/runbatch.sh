#!/bin/bash
# Flash the current tree, then capture N bench runs of one clip back-to-back.
# The workhorse of device run-to-run experiments (agreement = run pairs on the
# one earring; see tools/devpair.py for the scorer).
#
#   runbatch.sh RUNS OUTPREFIX [WAV] [SEC]
#
# Forces the LED-noise knob to 0 first (experiments measure the algorithm, not
# the blanking) and refuses to run against a missing WAV. Never edit pipeline
# source while this runs: capture2.py resolves symbols from firmware.elf.
set -e
DIR="$(cd "$(dirname "$0")/.." && pwd)"
RUNS=$1; OUT=$2; WAV=${3:-/tmp/psync/h900.wav}; SEC=${4:-55}
[ -f "$WAV" ] || { echo "no WAV $WAV" >&2; exit 1; }
[ -n "$RUNS" ] && [ -n "$OUT" ] || { echo "usage: runbatch.sh RUNS OUTPREFIX [WAV] [SEC]" >&2; exit 1; }
cd "$DIR"
python3 - "$DIR/sync.go" <<'PY'
import re, sys
s = open(sys.argv[1]).read()
open(sys.argv[1], "w").write(re.sub(r"const benchLedLevel = \d+", "const benchLedLevel = 0", s, count=1))
PY
tinygo build -o firmware.hex -target=./stm32l0x1.json -opt=2 -panic=trap .
tinygo build -o firmware.elf -target=./stm32l0x1.json -opt=2 -panic=trap .
# Flash must report Verified OK - do not burn a batch of captures on a board
# that is off its contacts ("init mode failed" with target voltage present).
flash_ok=""
for try in 1 2 3; do
  pkill -f openocd || true; sleep 0.5
  if openocd -f interface/stlink.cfg -f target/stm32l0.cfg -c "program firmware.hex verify reset exit" 2>&1 | grep -q "Verified OK"; then
    flash_ok=1; break
  fi
  echo "flash attempt $try failed (board loose on the pogo pins?)" >&2
done
[ -n "$flash_ok" ] || { echo "ABORT: cannot flash" >&2; exit 1; }
echo "flash: Verified OK"
for r in $(seq 1 "$RUNS"); do
  for try in 1 2 3; do
    if python3 tools/capture2.py --file "$WAV" --out "${OUT}_r${r}.csv" --seconds "$SEC" 2>&1 | tail -1; then
      break
    fi
    echo "  capture retry $try"; pkill -f openocd || true; sleep 1
  done
done
