#!/usr/bin/env python3
"""Score one device run against another: identity agreement + anchor F1.

The two things the sync actually depends on, read straight off telemetry CSVs
(tools/capture2.py):

  ident    Hamming distance over the IDENTITY fields (shape+hot, bits 8..29),
           minimised over a +-15 frame alignment search. This is what the ring
           compares after the identity-only matching change; the temporal field
           is excluded because it tracks push phase, not the section.
  <=3      the fraction of frames inside DAccept, the device's own tolerance.
  pat      animation-pattern agreement (what the user sees).
  beat+-2  beat-boundary F1 at +-2 frames.
  anchors  anchors per run and their cross-run F1 at +-30 frames, the
           agreement_test convention (devices lock at different instants, so
           the musical transition must match, not the frame).

Usage: devpair.py A.csv B.csv [C.csv ...]   (first vs each of the rest)
"""
import csv
import sys


def load(path):
    rows = [r for r in csv.DictReader(open(path)) if r["kind"] == "T"]
    g = lambda k: [r[k] for r in rows]  # noqa: E731
    return dict(
        hash=[int(v, 16) for v in g("hash")],
        lock=[v == "True" for v in g("locked")],
        pat=[int(v) for v in g("pattern")],
        ret=[v == "True" for v in g("returned")] if "returned" in rows[0] else [False] * len(rows),
        beat=[v == "True" for v in g("beat")],
        anch=[v == "True" for v in g("anchor")],
        per=[int(v) for v in g("period")],
    )


IDENT = 0x3FFFFF00


def popcount(x):
    n = 0
    while x:
        n += x & 1
        x >>= 1
    return n


def best_align(a, b, mask=IDENT):
    """(mean identity distance, lag, fraction within DAccept) at best alignment."""
    best = (99.0, 0, 0.0)
    for lag in range(-15, 16):
        sa = lag if lag >= 0 else 0
        sb = 0 if lag >= 0 else -lag
        n = min(len(a["hash"]) - sa, len(b["hash"]) - sb)
        d = [popcount((a["hash"][sa + i] ^ b["hash"][sb + i]) & mask) for i in range(n)]
        v = [(a["lock"][sa + i] and b["lock"][sb + i]) for i in range(n)]
        sel = [d[i] for i in range(n) if v[i]]
        if not sel:
            continue
        mean = sum(sel) / len(sel)
        if mean < best[0]:
            best = (mean, lag, sum(1 for x in sel if x <= 3) / len(sel))
    return best


def align_lag(a, b):
    return best_align(a, b)[1]


def event_f1(ia, ib, tol):
    """Greedy nearest-match F1 between two event index lists."""
    used = [False] * len(ib)
    m = 0
    for i in ia:
        bd, bj = tol + 1, -1
        for j, k in enumerate(ib):
            if not used[j] and abs(i - k) < bd:
                bd, bj = abs(i - k), j
        if bj >= 0:
            used[bj] = True
            m += 1
    return m, 2 * m / max(len(ia) + len(ib), 1)


def anchor_f1(a, b, lag=0, tol=30):
    # Run B's frame indices are shifted by the alignment lag: run B frame j is
    # run A frame j+lag, so B's anchors move into A's frame numbering by +lag.
    ia = [i for i, x in enumerate(a["anch"]) if x]
    ib = [i + lag for i, x in enumerate(b["anch"]) if x]
    used = [False] * len(ib)
    m = 0
    for i in ia:
        bd, bj = tol + 1, -1
        for j, k in enumerate(ib):
            if not used[j] and abs(i - k) < bd:
                bd, bj = abs(i - k), j
        if bj >= 0:
            used[bj] = True
            m += 1
    return len(ia), len(ib), m, 2 * m / max(len(ia) + len(ib), 1)


def median(vals):
    s = sorted(vals)
    return s[len(s) // 2] if s else 0


def score(pa, pb):
    A, B = load(pa), load(pb)
    mean, lag, le3 = best_align(A, B)
    sa = lag if lag >= 0 else 0
    sb = 0 if lag >= 0 else -lag
    n = min(len(A["hash"]) - sa, len(B["hash"]) - sb)
    v = [A["lock"][sa + i] and B["lock"][sb + i] for i in range(n)]
    pat = sum(1 for i in range(n) if v[i] and A["pat"][sa + i] == B["pat"][sb + i]) / max(sum(v), 1)
    ba = [i for i, x in enumerate(A["beat"][sa : sa + n]) if x]
    bb = [i for i, x in enumerate(B["beat"][sb : sb + n]) if x]
    bf = (
        sum(1 for k in ba if min(abs(j - k) for j in bb) <= 2) / len(ba)
        if ba and bb
        else 0.0
    )
    na, nb, mm, f1 = anchor_f1(A, B, lag)
    per_a = median([p for p, l in zip(A["per"], A["lock"]) if l])
    per_b = median([p for p, l in zip(B["per"], B["lock"]) if l])
    # Periodicity confirmation: how often the ring match fires, and whether the
    # two runs agree about *when* it fires. Many more events than anchors, so
    # this is the statistically meaningful read on the matching change.
    ra = [i for i, x in enumerate(A["ret"]) if x]
    rb = [i + lag for i, x in enumerate(B["ret"]) if x]
    _, rf1 = event_f1(ra, rb, 30)
    nlock = max(sum(A["lock"]), 1)
    return dict(ident=mean, le3=le3, pat=pat, beat=bf, na=na, nb=nb, af1=f1,
                per_a=per_a, per_b=per_b, ret_a=len(ra), ret_b=len(rb),
                ret_rate=len(ra) / nlock, ret_f1=rf1)


def main():
    paths = sys.argv[1:]
    if len(paths) < 2:
        sys.exit(__doc__)
    print("pair                          ident  <=3    pat  beat+-2  anch  aF1   "
          "ret-run1 ret-run2  ret%  retF1  per")
    for p in paths[1:]:
        s = score(paths[0], p)
        print(
            "%-28s %5.2f %5.0f%% %5.1f%% %7.1f%% %3d/%-3d %.2f %7d %8d %6.1f%% "
            "%5.2f %3d/%d"
            % (
                f"{paths[0].split('/')[-1]}~{p.split('/')[-1]}",
                s["ident"],
                100 * s["le3"],
                100 * s["pat"],
                100 * s["beat"],
                s["na"],
                s["nb"],
                s["af1"],
                s["ret_a"],
                s["ret_b"],
                100 * s["ret_rate"],
                s["ret_f1"],
                s["per_a"],
                s["per_b"],
            )
        )


if __name__ == "__main__":
    main()
