#!/usr/bin/env python3
"""Score a pair capture the way a person looking at the earrings would.

paircapture.py reports instantaneous pattern agreement ("pat %"), and that number
is misleading in both directions: a selector frozen on one band agrees with
itself perfectly, and a fast one can average 60% while never settling long enough
for either earring's colour to be read. What a viewer actually reports is how
long the two match before one of them moves. So this scores runs of at least HOLD
seconds and reports the two sides separately:

  same  fraction of the capture inside a matching stretch of >= HOLD s
  diff  the mirror image - inside a disagreeing stretch of >= HOLD s, which is
        the complaint ("they often don't have the same colour")

and prints the change rate and pattern histogram per device, which is how a
frozen selector gets caught: high agreement from a selector that never changes is
worth nothing, so a run is only worth believing with both devices live
(>= 1 change per CHANGE_S seconds) and all patterns represented.

The two files are matched by nearest timestamp. Each device is polled by its own
thread with its own clock origin, and t is sampled once per poll (~9 Hz) while
rows arrive at the analysis rate (~62 Hz), so consecutive rows repeat a t and the
timelines are offset by whatever the thread start difference was. Nearest match
lands within 60 ms, a small fraction of any hold window worth scoring.

Usage: python3 tools/pairscore.py DIR [HOLD_S ...]
       python3 tools/pairscore.py /tmp/psync/gt8 1 2 3
"""
import bisect
import csv
import sys


def load(path, name):
    rows = [r for r in csv.DictReader(open(f"{path}/{name}.csv")) if r["kind"] == "T"]
    return {
        "t": [float(r["t"]) for r in rows],
        "p": [int(r["pattern"]) for r in rows],
        "h": [int(r["hash"], 16) for r in rows],
        "l": [r["locked"] == "True" for r in rows],
    }


def match(a, b):
    """Index in b nearest to each entry of a, both sorted by time."""
    out = []
    for t in a["t"]:
        k = min(bisect.bisect_left(b["t"], t), len(b["t"]) - 1)
        if k > 0 and abs(b["t"][k - 1] - t) < abs(b["t"][k] - t):
            k -= 1
        out.append(k)
    return out


def runs(same, fps, hold):
    """Frames inside a matching, and inside a mismatching, run of at least hold s."""
    n = len(same)
    edge = [0] + [i + 1 for i in range(n - 1) if same[i + 1] != same[i]] + [n]
    hit = miss = 0
    for lo, hi in zip(edge[:-1], edge[1:]):
        if (hi - lo) / fps >= hold:
            if same[lo]:
                hit += hi - lo
            else:
                miss += hi - lo
    return hit / n, miss / n


def main():
    path = sys.argv[1]
    holds = [float(x) for x in sys.argv[2:]] or [1.0, 2.0, 3.0]
    a, b = load(path, "L"), load(path, "R")
    idx = match(a, b)
    keep = [i for i, k in enumerate(idx) if abs(b["t"][k] - a["t"][i]) < 0.08]
    pa = [a["p"][i] for i in keep]
    pb = [b["p"][idx[i]] for i in keep]
    t = [a["t"][i] for i in keep]
    locked = [a["l"][i] and b["l"][idx[i]] for i in keep]
    dist = [bin(x ^ y).count("1")
            for x, y in zip((a["h"][i] for i in keep), (b["h"][idx[i]] for i in keep))]
    fps = len(t) / (t[-1] - t[0])
    dur = t[-1] - t[0]
    npat = max(max(pa), max(pb)) + 1
    print(f"{path}: {len(keep)} matched frames of {len(a)}+{len(b)}, {dur:.0f} s "
          f"at {fps:.1f} Hz, both locked {100 * sum(locked) / len(locked):.0f}%, "
          f"hash dist<=3 {100 * sum(1 for d in dist if d <= 3) / len(dist):.0f}%")
    print(f"  instantaneous agreement "
          f"{100 * sum(1 for x, y in zip(pa, pb) if x == y) / len(pa):.1f}%")
    for h in holds:
        same, diff = runs([x == y for x, y in zip(pa, pb)], fps, h)
        print(f"  same >= {h:.0f}s {100 * same:5.1f}%   "
              f"stuck-different >= {h:.0f}s {100 * diff:5.1f}%   "
              f"score {same - diff:+.3f}")
    for name, p in (("L", pa), ("R", pb)):
        ch = sum(1 for i in range(1, len(p)) if p[i] != p[i - 1]) / dur
        print(f"  {name}: {ch:.2f} changes/s  counts {[p.count(i) for i in range(npat)]}")
    print(f"  patterns seen {npat}, missing "
          f"{[i for i in range(npat) if i not in set(pa) & set(pb)]}")


main()
