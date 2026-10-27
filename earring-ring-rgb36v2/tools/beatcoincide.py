#!/usr/bin/env python3
"""Do the two earrings agree about WHEN, even if they disagree about WHAT?

The spectral features carry nothing shared: centroid, band ratios and smoothed forms
of both all score at or below chance across a pair, however they are normalised.
Before replacing the feature it is worth knowing whether ANYTHING is shared, and the
candidate is timing. Both units run the same beat tracker on the same music, and a
beat is a time-domain event that does not depend on where a microphone sits the way a
spectral tilt does. If the locks coincide, a colour advanced on a shared musical event
matches by construction and no feature has to agree about anything.

Getting a timestamp fine enough to ask that is most of this tool. The telemetry poller
drains a 40-frame ring roughly every 0.8 s and stamps the whole drain with one host
time, so `t` has about 0.8 s of resolution and cannot resolve a beat: measuring
coincidence with it directly yields a flat distribution and a confident "the locks do
not agree". But within a drain the rows carry consecutive sequence numbers, one per
analysis frame, so each row's real time is the drain time minus how many frames behind
that drain's newest row it was:

    t_frame = t_drain - (seq_newest_in_drain - seq) / 62.5

Every drain re-anchors to the host clock, so this does not accumulate the devices'
clock difference and is good to a few milliseconds. Nothing is fitted between the two
devices, so there is no free parameter that could manufacture a coincidence.

  python3 tools/beatcoincide.py DIR
"""
import csv
import os
import sys

import numpy as np

FRAME_HZ = 62.5  # syncdsp.Window = 250 at 16 kSPS
EVENTS = ("onset", "beat", "bar", "anchor", "pchange")


def read_rows(path):
    """Rows as dicts, with the header taken from the first line.

    An earlier version of this joined the first two lines, because a terminal had
    wrapped the header and made it look like a continuation. It is one line of 27
    names. Rather than trusting that either way, the length of the first data row
    decides: a header that does not match the data is the failure mode, and joining
    a second line that is not a header shifts every name by one, which reads the vote
    count as a tempo and never complains.
    """
    with open(path) as fh:
        rows = [p for p in csv.reader(fh) if p]
    if len(rows) < 2:
        return []
    header = rows[0]
    body = rows[1:]
    if len(header) != len(body[0]):
        # Only now consider a continuation line, and only if it fixes the count.
        joined = rows[0] + rows[1][1:]
        if len(joined) == len(body[1] if len(body) > 1 else body[0]):
            header, body = joined, body[1:]
        else:
            raise SystemExit(
                f"{path}: header has {len(header)} names, data rows have "
                f"{len(body[0])} fields, and joining the second line does not fix "
                f"it. Refusing to guess a column mapping.")
    return [dict(zip(header, p)) for p in body if p and p[0] == "T"]


def load(path):
    rows = read_rows(path)
    drain = {}
    for r in rows:
        try:
            t, seq = float(r["t"]), int(r["seq"])
        except (KeyError, TypeError, ValueError):
            continue
        drain[t] = max(drain.get(t, seq), seq)
    out = {k: [] for k in EVENTS}
    periods, locked = [], []
    for r in rows:
        try:
            t, seq = float(r["t"]), int(r["seq"])
        except (KeyError, TypeError, ValueError):
            continue
        tf = t - (drain[t] - seq) / FRAME_HZ
        for k in EVENTS:
            if r.get(k, "").strip() == "True":
                out[k].append(tf)
        try:
            p = int(r.get("period", "").strip())
            if p > 0:
                periods.append(p)
        except ValueError:
            pass
        if r.get("locked", "").strip() == "True":
            locked.append(tf)
    return ({k: np.sort(np.asarray(v)) for k, v in out.items()},
            np.asarray(periods, dtype=float), np.asarray(locked, dtype=float),
            len(rows))


def deltas(a, b):
    if len(a) == 0 or len(b) == 0:
        return np.asarray([])
    i = np.clip(np.searchsorted(b, a), 0, len(b) - 1)
    j = np.clip(i - 1, 0, len(b) - 1)
    da, db = b[i] - a, a - b[j]
    return np.where(np.abs(da) <= np.abs(db), da, -db)


def main():
    d = sys.argv[1]
    ev, per, lck, nrows = {}, {}, {}, {}
    for side in ("L", "R"):
        for c in (f"{side}.csv", f"telem-{side}.csv"):
            p = os.path.join(d, c)
            if os.path.exists(p):
                ev[side], per[side], lck[side], nrows[side] = load(p)
                break
        else:
            raise SystemExit(f"{d}: no telemetry for {side}")

    lo = max(ev["L"]["onset"][0], ev["R"]["onset"][0])
    hi = min(ev["L"]["onset"][-1], ev["R"]["onset"][-1])
    print(f"{d}: {(hi-lo)/60:.1f} min of reconstructed event times")
    for side in ("L", "R"):
        print(f"  {side}: {nrows[side]} frames, locked on "
              f"{len(lck[side])*100.0/max(1, nrows[side]):.0f}%, "
              f"{len(ev[side]['onset'])} onsets, {len(ev[side]['beat'])} beats, "
              f"{len(ev[side]['bar'])} bars")

    beat = 0.5
    if len(per["L"]):
        beat = float(np.median(per["L"])) / FRAME_HZ
        print(f"  L's median claimed period: {np.median(per['L']):.0f} frames = "
              f"{beat*1000:.0f} ms = {60/beat:.1f} BPM")
    if len(per["R"]):
        print(f"  R's median claimed period: {np.median(per['R']):.0f} frames = "
              f"{60/(np.median(per['R'])/FRAME_HZ):.1f} BPM")

    print(f"\n  {'event':<9} {'n L':>6} {'n R':>6}  {'<=50ms':>7} {'<=150ms':>8} "
          f"{'<=500ms':>8} {'<=1 beat':>9}  {'median|dt|':>10}")
    for k in EVENTS:
        a = ev["L"][k][(ev["L"][k] >= lo) & (ev["L"][k] <= hi)]
        b = ev["R"][k][(ev["R"][k] >= lo) & (ev["R"][k] <= hi)]
        if len(a) < 5 or len(b) < 5:
            print(f"  {k:<9} {len(a):6d} {len(b):6d}   too few events to score")
            continue
        dl = np.abs(deltas(a, b))
        print(f"  {k:<9} {len(a):6d} {len(b):6d} {np.mean(dl <= 0.05)*100:6.1f}% "
              f"{np.mean(dl <= 0.15)*100:7.1f}% {np.mean(dl <= 0.5)*100:7.1f}% "
              f"{np.mean(dl <= beat)*100:8.1f}%  {np.median(dl)*1000:9.0f} ms")

    # Chance coincidence. If L's events were scattered at random through the same span,
    # how often would an R event happen to be nearby? Without this, "62% within one
    # frame" reads as success when events are dense enough that any placement does it.
    span = hi - lo
    for k in ("onset", "beat"):
        b = ev["R"][k][(ev["R"][k] >= lo) & (ev["R"][k] <= hi)]
        if len(b) < 5 or span <= 0:
            continue
        rate = len(b) / span
        for w in (0.05, 0.15, 0.5, beat):
            print(f"  chance of an R {k} within {w*1000:5.0f} ms of a random "
                  f"instant: {min(1.0, 2*w*rate)*100:5.1f}%")
    if len(per["L"]) and len(per["R"]):
        sl, sr = set(np.round(per["L"]).astype(int)) , set(np.round(per["R"]).astype(int))
        print(f"\n  tempo claimed by both: {sorted(sl & sr)} "
              f"({len(sl & sr)} shared, of {len(sl)} L and {len(sr)} R)")
    print("\n  150 ms is about nine analysis frames: that is what 'the same beat' has")
    print("  to mean here. Read every percentage against the chance line above it.")


if __name__ == "__main__":
    main()
