#!/usr/bin/env python3
"""Summarise a LED-noise sweep: per-level run-to-run recognition agreement.

  ledcurve.py /tmp/psync/L 0 64 128 192 256

Expects <prefix><level>_r{1,2}.csv from tools/ledsweep.sh. For every level it
runs tools/runpair.py over the level's runs and collects the numbers that
matter for "do the earrings still recognise the music": locked period (must be
stable AND identical between runs), exact hash agreement at the best alignment
(now that the hash has entropy this actually measures recognition), the
fraction of frames inside DAccept, pattern agreement, beat+-2 F1, and per-run
hash entropy + anchor count (the failure mode is the hash saturating or the
anchor storm, not just a lower score).
"""
import re
import subprocess
import sys
import os

HERE = os.path.dirname(os.path.abspath(__file__))


def runpair(paths):
    r = subprocess.run([sys.executable, os.path.join(HERE, "runpair.py")] + paths,
                       capture_output=True, text=True, timeout=300)
    out = r.stdout
    files = re.findall(r"^(\S+\.csv): (\d+) frames, lock ([\d]+)%, period (\d+), "
                       r"onsets (\d+), anchors (\d+), (\d+) hashes", out, re.M)
    pair = re.search(r"^\S+~\S+\s+(-?\d+)\s+(\d+)\s+([\d.]+)%\s+([\d.]+)\s+"
                     r"(\d+)\s+([\d.]+)%\s+([\d.]+)%\s+([\d.]+)%\s+([\d.]+)%\s+"
                     r"(\d+)/(\d+)", out, re.M)
    return files, pair


def main():
    prefix = sys.argv[1]
    levels = sys.argv[2:]
    print(f"{'lvl':>4} {'per(runs)':>10} {'agree':>5} {'hash=':>6} {'<=3':>6} "
          f"{'pat':>6} {'beat+-2':>7} {'anchors':>9} {'dist-hashes':>12}")
    for lvl in levels:
        paths = []
        n = 1
        while os.path.exists(f"{prefix}{lvl}_r{n}.csv"):
            paths.append(f"{prefix}{lvl}_r{n}.csv")
            n += 1
        if not paths:
            print(f"{lvl:>4}  (no captures)")
            continue
        files, pair = runpair(paths)
        pers = [f[3] for f in files]
        anch = [f[5] for f in files]
        hashes = [f[6] for f in files]
        same = "yes" if len(set(pers)) == 1 else "NO"
        if pair:
            lag, nf, he, ham, h95, le3, pat, b0, b2, p1, p2 = pair.groups()
            print(f"{lvl:>4} {'/'.join(pers):>10} {same:>5} {he:>5}% {le3:>5}% "
                  f"{pat:>5}% {b2:>6}% {','.join(anch):>9} {','.join(hashes):>12}")
        else:
            print(f"{lvl:>4} {'/'.join(pers):>10} {same:>5}  (no pair line: "
                  f"{len(paths)} runs?) {'- ':>5} {'-':>5} {'-':>5} "
                  f"{','.join(anch):>9} {','.join(hashes):>12}")


if __name__ == "__main__":
    main()
