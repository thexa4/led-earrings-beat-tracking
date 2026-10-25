#!/usr/bin/env python3
"""Choose the pattern cuts from the measured z distribution of the real pair.

PatternIndex places its cuts at the Gaussian quartiles in mean-absolute-deviation
units (PatternCutQ8 = 216/256 = 0.844). That is a defensible default for a
distribution nobody has measured, and a poor one once somebody has: the requirement
is that the four colours share the time roughly evenly, which is a statement about
the observed distribution, not about the Gaussian.

This reads the z trajectories that tools/patternstate.py sampled from the two
earrings' RAM and answers the only two questions that matter, together.

Superseded for anything but this one question by replay.py, which fits the cuts on
the first half of a paircapture --state capture and reports them on the second, and
searches them jointly with the reference and spread time constants instead of holding
those fixed at what the firmware happens to ship.

  * what would each candidate cut set do to the occupancy of each device;
  * what would it do to their agreement.

Both have to be reported because they trade against each other, and because either
one alone has a degenerate answer - one wide bucket gives perfect agreement and no
variety, and four arbitrary ones give even occupancy and none. The cut set is a
constant compiled into both devices, so unlike the rejected empirical quartile
tracker there is nothing for the two earrings to converge to independently: the
ruler cannot drift.
"""
import csv
import sys
from collections import Counter


def load(path, floor=0):
    """Return the paired z trajectories the shipped firmware would have used.

    patternstate.py samples cent, centAvg and centDev; the shipped mapping divides
    by centDev directly and clamps the deviation to twice it, so z is rebuilt here
    rather than read from the file's own column - the clamp has to be reproduced or
    this measures a ruler no device runs. The floor argument exists only so a
    candidate change can be evaluated against captures taken before it.
    """
    zs = {"L": [], "R": []}
    for row in csv.DictReader(open(path)):
        try:
            gap, dev = int(row["gap"]), int(row["dev"])
        except (KeyError, ValueError):
            continue
        spread = max(dev, floor)
        d = max(-2 * spread, min(2 * spread, gap))
        zs[row["side"]].append(d * 256 / spread / 256.0)
    n = min(len(zs["L"]), len(zs["R"]))
    return zs["L"][:n], zs["R"][:n]


def quantile(v, q):
    v = sorted(v)
    i = min(len(v) - 1, max(0, int(q * (len(v) - 1))))
    return v[i]


def bucket(z, cuts):
    b = 0
    for c in cuts:
        if z > c:
            b += 1
    return b


def stats(zs, cuts):
    h = Counter(bucket(z, cuts) for z in zs)
    n = len(zs)
    occ = [100 * h.get(i, 0) / n for i in range(len(cuts) + 1)]
    return occ


def agreement(a, b, cuts):
    n = len(a)
    same = sum(1 for x, y in zip(a, b) if bucket(x, cuts) == bucket(y, cuts))
    pa = [0.0] * (len(cuts) + 1)
    pb = [0.0] * (len(cuts) + 1)
    for x, y in zip(a, b):
        pa[bucket(x, cuts)] += 1
        pb[bucket(y, cuts)] += 1
    chance = sum(pa[i] / n * pb[i] / n for i in range(len(cuts) + 1))
    return 100 * same / n, (same / n - chance) / (1 - chance) if chance < 1 else 0


def evenness(occ):
    """1 - mean absolute deviation from a quarter, as a percentage."""
    return 100 - sum(abs(o - 25.0) for o in occ) / 2 * 2


def main():
    path = sys.argv[1] if len(sys.argv) > 1 else "/dev/stdin"
    zl, zr = load(path)
    if not zl:
        sys.exit("no z samples in " + path)
    print(f"{len(zl)} paired samples of z")
    for name, v in (("L", zl), ("R", zr)):
        print(f"  {name}: mean {sum(v)/len(v):+.3f}  quantiles "
              + " ".join(f"q{int(q*100)}={quantile(v,q):+.2f}"
                         for q in (0.05, 0.25, 0.5, 0.75, 0.95)))

    shipped = [-0.844, 0.0, 0.844]
    print("\ncuts are of the form [-k, 0, +k] because that is what the shipped mapping")
    print("derives from one constant: cut i = (2i - (N-2)) * PatternCutQ8 / 2.")
    print("Asymmetric cut sets are not implementable without changing that formula,\n"
          "so the only free parameter is k. PatternCutQ8 = 256 * k.\n")
    print("%-30s %-24s %-24s agree  kappa  least-used"
          % ("cuts", "L occupancy %", "R occupancy %"))
    rows = []
    for k in (0.4, 0.5, 0.6, 0.7, 0.844, 1.0, 1.2, 1.5):
        cuts = [-k, 0.0, k]
        ol, orr = stats(zl, cuts), stats(zr, cuts)
        ag, kap = agreement(zl, zr, cuts)
        least = min(min(ol), min(orr))
        rows.append((k, ol, orr, ag, kap, least))
    for k, ol, orr, ag, kap, least in rows:
        fmt = lambda o: " ".join(f"{x:4.0f}" for x in o)
        star = "  <- shipped" if abs(k - 0.844) < 1e-6 else ""
        print("k=%.3f %-21s %-24s %-24s %5.1f%%  %+.3f  %4.0f%%%s"
              % (k, "", fmt(ol), fmt(orr), ag, kap, least, star))

    best = max(rows, key=lambda r: (r[5], r[3]))
    print("\nBest least-used bucket: k=%.3f at %.1f%% agreement (min bucket %.0f%%)"
          % (best[0], best[3], best[5]))
    print("PatternCutQ8 would be %d (currently %d)."
          % (round(256 * best[0]), 216))


if __name__ == "__main__":
    main()
