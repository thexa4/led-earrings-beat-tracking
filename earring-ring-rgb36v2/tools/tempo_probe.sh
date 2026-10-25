#!/bin/bash
# Host-side tempo reliability probe: replay the bench-channel audio through the
# pipeline under many perturbations (capture-phase offsets, noise seeds) and
# report which period each run chooses plus the winning margin over the
# runner-up. Used to judge TempoWindow / evidence changes without the device.
#
#   tempo_probe.py [WAV ...]      (default: the ph*.wav phase set)
#
# Prints one line per input: file, chosen period, top-3 evidence, margin%.
set -e
cd /home/max/Documents/led-earrings/debug/earring-ring-rgb36v2
FILES=${@:-$(ls /tmp/psync/ph*.wav)}
for f in $FILES; do
  out=$(HOSTBEAT=$f go test -count=1 -v -run TestReplayWav ./syncdsp/ 2>&1 |
    grep -E "^frames=|^ev:" | tr '\n' ' ')
  per=$(echo "$out" | sed -n 's/.* period=\([0-9]*\).*/\1/p')
  ev=$(echo "$out" | sed -n 's/.*ev: \(.*\)/\1/p')
  top=$(echo "$ev" | awk '{print $2}' | cut -d: -f2)
  nxt=$(echo "$ev" | awk '{print $3}' | cut -d: -f2)
  margin=$(python3 -c "a,b='$top','$nxt'; a=0 if a=='' else float(a); b=0 if b=='' else float(b); print(f'{100*(a-b)/a:.0f}' if a else '-')")
  printf "%-24s period=%-3s margin=%-4s  ev: %s\n" "$(basename $f)" "$per" "${margin}%" "$ev"
done
