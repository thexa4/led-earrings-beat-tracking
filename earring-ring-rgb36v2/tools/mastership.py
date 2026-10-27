#!/usr/bin/env python3
"""Replay master's REAL commit path - beats, refractory and all - and price the
constants a firmware change could actually make.

Frame-level agreement numbers are misleading for master, because master does not
change colour every frame. It commits a pattern at a beat or an anchor, at most
once every PatternRefractoryBeats beats, and only while the beat tracker is
locked. So a per-frame replay of usePattern measures a signal nobody sees: the
device holds its colour between commits, and the commits are what a person in the
room perceives. Every margin conclusion drawn from a frame-level replay is there-
fore suspect, including ones drawn earlier in this project.

This replays the commit path as written:

  decide()   on a hash push, if (returned or bar/beat) and at least
             PatternRefractoryBeats beats since the last change, commit
             usePattern(dev)
  anchor     a level trigger on onset + novelty + rising energy, deliberately
             device-independent, also committing usePattern(dev)

The beat flags, the lock state and the anchor flags come out of the capture, so
each device is gated by its OWN beat tracker - the same information the firmware
had - and the bands come from state_*.csv, which is p.smooth and therefore is
master's input verbatim: SmoothShift, Window, SampleRate and every AGC constant
are identical between the two branches (checked in both consts.go files), so the
debug-branch capture is a legitimate input to master's selector.

The reference point is the debug firmware's own displayed pattern on the same
capture, read out of the device over SWD: kappa +0.46 on an 11.6 minute capture
of loud EDM, four patterns, 1.5 changes per minute. That is what a real earring
pair did, not a replay, and it is the bar any master change has to be described
against.

  python3 mastership.py ~/psync/goodA [more captures...]
"""
import csv
import os
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import blocksel as bs  # noqa: E402  grid, devs_all, argmax_all, slow_bands
import ema as emalib  # noqa: E402

NB = 8
FPS = 62.5
NP = NB - 1
# syncdsp/consts.go on master.
REFRACTORY_BEATS = 4
ANCHOR_ONSET_MAX = 20
WARMUP = 64


def events(path):
    """Per-frame event flags from a telemetry dump (two-line header, kind T)."""
    with open(path) as fh:
        rd = csv.reader(fh)
        names = next(rd)
        nxt = next(rd)
        if nxt and not nxt[0]:
            names = [a if a else b for a, b in zip(names, nxt)]
            body = list(rd)
        else:
            body = [nxt] + list(rd)
    body = [r for r in body if r and r[0] == "T"]
    idx = {n: i for i, n in enumerate(names)}

    def flag(n):
        i = idx.get(n)
        if i is None:
            return np.zeros(len(body), bool)
        return np.array([r[i].strip() == "True" for r in body])

    def num(n, default=0):
        i = idx.get(n)
        out = []
        for r in body:
            v = r[i] if i is not None and len(r) > i else ""
            try:
                out.append(int(float(v)))
            except ValueError:
                out.append(default)
        return np.array(out, dtype=np.int64)

    t = np.array([float(r[idx["t"]]) for r in body])
    o = np.argsort(t, kind="stable")
    return dict(t=t[o],
                onset=flag("onset")[o], beat=flag("beat")[o],
                bar=flag("bar")[o], anchor=flag("anchor")[o],
                returned=flag("returned")[o], locked=flag("locked")[o],
                silent=flag("silent")[o], pushed=flag("pushed")[o],
                onset_age=num("onset_age", 999)[o],
                pattern=num("pattern", -1)[o])


def band_grid(path):
    """p.smooth as recorded: (times, bands).

    The state poller runs at roughly 5 Hz, not at the 62.5 Hz frame rate, so the
    bands are a slow sampling of a signal whose own time constant is 0.5 s.
    Interpolating that onto the event grid loses almost nothing; pretending it is
    per-frame would be the lie.
    """
    t, B = bs.load(path)
    return t.astype(np.float64), B


def onto(t, ev, bt, bands, grid, dt):
    """Put one device's per-frame streams on a common uniform grid.

    The two devices are polled independently, so their frame timestamps differ by
    a fraction of a frame and their streams end up different lengths. Comparing
    them index-by-index after that compares different frames.

    Pulse flags are OR-reduced over each target bucket rather than sampled: a
    beat is one frame long, and nearest-neighbour resampling drops it whenever
    the two grids fall between two pulses.
    """
    k = np.clip(((t - grid[0]) / dt).astype(np.int64), 0, len(grid) - 1)
    n = len(grid)

    def pulse(name):
        return np.bincount(k[ev[name]], minlength=n) > 0

    out = dict(beat=pulse("beat"), bar=pulse("bar"), pushed=pulse("pushed"),
               returned=pulse("returned"), anchor=pulse("anchor"),
               silent=pulse("silent"))
    # State, not pulses: previous value. For onset age the freshest (smallest)
    # value in the bucket, since the anchor gate wants the newest onset.
    idx = np.clip(np.searchsorted(t, grid, side="right") - 1, 0, len(t) - 1)
    out["locked"] = ev["locked"][idx]
    a = np.full(n, 1 << 30, dtype=np.int64)
    np.minimum.at(a, k, np.where(ev["onset_age"] < 900, ev["onset_age"], 1 << 30))
    out["onset_age"] = a
    out["pattern"] = ev["pattern"][idx]
    b = np.stack([np.interp(grid, bt, bands[:, i]) for i in range(NB)], 1)
    return out, b.round().astype(np.int64)


def replay(E, D, margin, refractory=REFRACTORY_BEATS, slow=0, G=None):
    """master's commit paths over a captured band stream + event stream."""
    if slow:
        D = bs.devs_all(bs.slow_bands(np.asarray(G, dtype=np.int64), slow))
    am = bs.argmax_all(D)
    n = len(D)
    out = np.zeros(n, dtype=np.int64)
    beat, bar = E["beat"], E["bar"]
    pushed, returned = E["pushed"], E["returned"]
    locked, anchor = E["locked"], E["anchor"]
    silent, age = E["silent"], E["onset_age"]
    del beat  # the beat count is inside decide(), see below
    disp = 0  # lastPattern: what the LEDs actually show
    band, valid = 0, False
    since = 0  # beatsSincePattern, a Go int32 starting at zero
    for i in range(n):
        if i < WARMUP or silent[i]:
            out[i] = disp
            continue
        # usePattern runs on every decide() and on every anchor, not only when a
        # pattern is committed: the incumbent advances even while the LEDs hold.
        want = int(am[i])
        if valid and want != band and D[i][want] < D[i][band] + margin:
            want = band
        band, valid = want, True
        if pushed[i]:
            # decide(): the refractory counter advances once per decide, and
            # setPattern only resets it when the pattern really changed.
            if (returned[i] or bar[i]) and since >= refractory and disp != band:
                disp = band
                since = 0
            since += 1
        if anchor[i] and age[i] <= ANCHOR_ONSET_MAX and disp != band:
            # The anchor snaps, ignoring the refractory, and resets it.
            disp = band
            since = 0
        out[i] = disp
    return out


def score(a, b, warm, dt):
    """Agreement over a grid whose sample spacing is dt seconds, not 1/FPS.

    Rates and hold times are computed from real elapsed time because the grid is
    the event poll grid, whose spacing is whatever the SWD read took.
    """
    a, b = a[warm:], b[warm:]
    seen = sorted(set(a.tolist()) | set(b.tolist()))
    po = float(np.mean(a == b))
    pe = sum(float(np.mean(a == k)) * float(np.mean(b == k)) for k in range(NP))
    kap = (po - pe) / (1 - pe) if pe < 1 else 0.0
    occ = [min(float(np.mean(a == k)), float(np.mean(b == k))) for k in seen] \
        or [0.0]
    secs = len(a) * dt
    ch = [float(np.count_nonzero(np.diff(p) != 0)) / secs * 60 for p in (a, b)]
    hold = []
    for p in (a, b):
        e = np.concatenate(([0], np.flatnonzero(np.diff(p) != 0) + 1, [len(p)]))
        hold.append(float(np.mean(np.diff(e))) * dt)
    return po, kap, min(occ), min(ch), min(hold), len(seen)


def main():
    emalib.self_test()
    caps = []
    for d in sys.argv[1:]:
        EL, ER = events(os.path.join(d, "L.csv")), events(os.path.join(d, "R.csv"))
        btL, bL = band_grid(os.path.join(d, "state_L.csv"))
        btR, bR = band_grid(os.path.join(d, "state_R.csv"))
        # One clock, one grid: the event rows are one per analysis frame, so they
        # are the grid, and the slower-sampled bands are interpolated onto it.
        lo = max(EL["t"][0], ER["t"][0], btL[0], btR[0])
        hi = min(EL["t"][-1], ER["t"][-1], btL[-1], btR[-1])
        keepL = (EL["t"] >= lo) & (EL["t"] <= hi)
        keepR = (ER["t"] >= lo) & (ER["t"] <= hi)
        evL = dict(EL)
        evR = dict(ER)
        evL = {k2: v[keepL] for k2, v in evL.items()}
        evR = {k2: v[keepR] for k2, v in evR.items()}
        dt = 1.0 / FPS
        grid = np.arange(lo, hi, dt)
        evL, GL = onto(evL["t"], evL, btL, bL, grid, dt)
        evR, GR = onto(evR["t"], evR, btR, bR, grid, dt)
        evL["t"], evR["t"] = grid, grid
        caps.append((d, GL, evL, GR, evR, dt))
        print(f"loaded {d}: {(hi-lo)/60:.1f} min, {len(grid)} grid frames at "
              f"{dt*1000:.0f} ms from {int(keepL.sum())}/{int(keepR.sum())} "
              f"captured frames")
    warm = int(60 / caps[0][5])

    hdr = "  ".join(f"{'agree  kappa minocc ch/min  hold used':>38}" for _ in caps)
    print(f"\n=== master's commit path (beats, refractory, anchors), "
          f"warm {warm/FPS:.0f} s ===")
    # Event rates, before any candidate: if these are not musically plausible
    # (a bar every couple of seconds, a handful of anchors a minute), the replay
    # is fed the wrong flags and every number below it is meaningless.
    print("  event rates per minute, from the capture:")
    for d, GL, EL, GR, ER, dt in caps:
        mins = (len(GL) - warm) * dt / 60
        for tag, ev in (("L", EL), ("R", ER)):
            r = {k: float(np.count_nonzero(ev[k][warm:])) / mins
                 for k in ("pushed", "bar", "returned", "anchor")}
            lk = float(np.mean(ev["locked"][warm:])) * 100
            print(f"    {os.path.basename(d) if tag == 'L' else '':<12} {tag} " +
                  " ".join(f"{k} {v:6.1f}" for k, v in r.items()) +
                  f"  locked {lk:5.1f}%")

    print(f"  {'variant':<26} {hdr}")
    cands = []
    for m in (0, 4, 8, 16, 32, 64):
        cands.append((f"margin {m}<<8", m, 0))
    for m in (8, 16, 32):
        cands.append((f"margin {m}<<8 slow0.26s", m, 4))
    for name, margin, slow in cands:
        cells = []
        for d, GL, EL, GR, ER, dt in caps:
            a = replay(EL, bs.devs_all(GL), margin, slow=slow, G=GL)
            b = replay(ER, bs.devs_all(GR), margin, slow=slow, G=GR)
            po, kap, mo, ch, hold, used = score(a, b, warm, dt)
            cells.append(f"{po*100:5.1f}% {kap:+6.3f} {mo*100:5.1f}% "
                         f"{ch:6.1f} {hold:5.1f} {used:3d}")
        tag = "   <-- shipped" if name == "margin 32<<8" else ""
        print(f"  {name:<26} " + "  ".join(cells) + tag, flush=True)

    print(f"\n  reference: the debug firmware's OWN displayed pattern, read from "
          f"the chip")
    for d, GL, EL, GR, ER, dt in caps:
        a = EL["pattern"].astype(np.int64)
        b = ER["pattern"].astype(np.int64)
        ok = (a >= 0) & (b >= 0)
        a, b = a[ok][warm:], b[ok][warm:]
        if len(a) < 1000:
            print(f"    {os.path.basename(d)}: no pattern column")
            continue
        po = float(np.mean(a == b))
        pe = sum(float(np.mean(a == k)) * float(np.mean(b == k))
                 for k in range(int(max(a.max(), b.max())) + 1))
        ch = [float(np.count_nonzero(np.diff(p) != 0)) / (len(p) * dt) * 60
              for p in (a, b)]
        print(f"    {os.path.basename(d)}: {po*100:5.1f}% "
              f"kappa {(po-pe)/(1-pe) if pe < 1 else 0:+.3f} "
              f"(4 colours, {min(ch):.1f} changes/min)")


if __name__ == "__main__":
    main()
