#!/usr/bin/env python3
"""Score a device run batch: per-run dominant period + all-run-pair agreement.

  batchscore.py GLOB_OR_FILES...

Prints each run's dominant locked period and switch count, then pair stats
(period agreement, identity distance, pattern, anchor F1 and returned F1
conditioned on same-period pairs, where frame indices are comparable at all).
Companion of runbatch.sh / devpair.py.
"""
import csv
import glob
import itertools
import statistics
import sys
from collections import Counter

sys.path.insert(0, __file__.rsplit("/", 1)[0])
import devpair as dp


def load(path):
    rows = [r for r in csv.DictReader(open(path)) if r["kind"] == "T"]
    return rows


def dominant(rows):
    lock = [r for r in rows if r["locked"] == "True"]
    if not lock:
        return None, 0, []
    c = Counter(int(r["period"]) for r in lock)
    timeline = [int(r["period"]) for r in lock if r["beat"] == "True"]
    switches = sum(1 for a, b in zip(timeline, timeline[1:]) if a != b)
    return c.most_common(1)[0][0], switches, timeline


def main():
    files = []
    for a in sys.argv[1:]:
        files += sorted(glob.glob(a)) if any(c in a for c in "*?[") else [a]
    pers, switch = [], []
    for f in files:
        rows = load(f)
        p, sw, _ = dominant(rows)
        pers.append(p)
        switch.append(sw)
        n_anch = sum(r["anchor"] == "True" for r in rows)
        print(f"{f.split('/')[-1]:16s} period={str(p):4s} switches={sw:<2d} anchors={n_anch}")
    print(f"per-run periods: {dict(Counter(pers))}   "
          f"switches total={sum(switch)} over {len(files)} runs")
    ident, pat, af1, rf1, nsp = [], [], [], [], 0
    for a, b in itertools.combinations(files, 2):
        s = dp.score(a, b)
        ident.append(s["ident"])
        pat.append(s["pat"])
        if dominant(load(a))[0] == dominant(load(b))[0]:
            nsp += 1
            af1.append(s["af1"])
            rf1.append(s["ret_f1"])
    same = sum(1 for a, b in itertools.combinations(pers, 2) if a == b)
    tot = len(pers) * (len(pers) - 1) // 2
    m = statistics.median
    print(f"pairs {tot}: period agreement {same}/{tot}")
    print(f"  identity dist med {m(ident):.2f}   pattern med {100*m(pat):.0f}%")
    if af1:
        print(f"  same-period pairs {nsp}: anchor F1 med {m(af1):.2f} "
              f"(vals {sorted(round(x,2) for x in af1)})")
        print(f"  returned F1 med {m(rf1):.2f}")


if __name__ == "__main__":
    main()
