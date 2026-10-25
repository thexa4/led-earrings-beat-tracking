#!/usr/bin/env python3
"""Score beat output against the music, not against another run.

Comparing two runs' beat trains to each other flatters any tracker that mostly
ignores the audio: a free-running grid agrees with itself perfectly. The honest
reference is the played file, and the pipeline's own host replay of that file is
the bridge between the two:

  host replay   run the pipeline over the WAV the speaker played
                (HOSTBEAT=... REPLAY_CSV=... go test -run TestReplayWav).
                Its frame f is samples f*250..f*250+249 of the file, so its beat
                grid *is* the music's grid - no alignment needed.
  device runs   telemetry CSVs, which start at an arbitrary moment of playback.
                Aligned to the host trace by cross-correlating the pipeline's
                per-frame loudness, which is the same quantity on both sides, so
                the correlation is near 1 and the offset is unambiguous. (An
                earlier version correlated against a filtered envelope instead
                and got 0.1-0.3, which made every number below meaningless.)

Then, per capture: how far each emitted beat sits from the true grid, and how
many true beats are covered.

Usage:
  truebeat.py HOST.csv CAPTURE1.csv [CAPTURE2 ...] [--window N]
"""
import sys

import numpy as np

W = 250
LO, HI = 18, 49


def read_csv(path):
    lines = [l.strip().split(",") for l in open(path) if l.strip()]
    head = lines[0]
    rows = [r for r in lines[1:] if len(r) == len(head)]
    out = {}
    for i, k in enumerate(head):
        col = [r[i] for r in rows]
        if k in ("beat", "onset", "locked", "anchor", "bar", "bass", "silent",
                 "pchange", "pushed"):
            out[k] = np.array([v in ("True", "true") for v in col])
        else:
            v = []
            for x in col:
                try:
                    v.append(float(x))
                except ValueError:
                    v.append(0.0)
            out[k] = np.array(v)
    return out


def true_grid(host):
    """(period, phase) from the host replay's own bass envelope.

    rawbass (the pre-normalisation bass) used to be the cleanest kick proxy but
    the debug column was dropped to fit the firmware in flash; basslog is the
    smoothed log-band bass that is still emitted and tracks the kick well
    enough for a reference grid."""
    e = host["basslog"]
    v = e - np.percentile(e, 10)
    v = v - v.mean()
    ac = np.correlate(v, v, "full")[len(v) - 1:]
    ac /= ac[0] + 1e-12
    p = LO + int(np.argmax(ac[LO:HI + 1]))
    win = min(2048, len(e))
    best = (0, -1e18)
    for ph in range(p):
        s = float(e[:win][np.arange(ph, win, p)].sum())
        if s > best[1]:
            best = (ph, s)
    return p, best[0], ac


def align(a, b, lag=600):
    """Offset of a relative to b, by correlating pipeline loudness."""
    x0 = a["total"]
    y = b["total"]
    y = y - y.mean()
    best = (0, -2.0)
    for d in range(-lag, lag + 1):
        s = max(0, d)
        e = min(len(y), len(x0) + d)
        if e - s < 800:
            continue
        x = x0[s - d:e - d]
        x = x - x.mean()
        c = float((x * y[s:e]).sum() / (np.linalg.norm(x) * np.linalg.norm(y[s:e]) + 1e-9))
        if c > best[1]:
            best = (d, c)
    return best


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    host = read_csv(args[0])
    p, ph, ac = true_grid(host)
    print(f"{args[0]}: music grid = period {p} frames ({60*64/p:.1f} BPM), "
          f"phase {ph}, autocorrelation {ac[p]:.2f}")
    print(f"{'capture':<20} {'corr':>5} {'n':>5} {'period':>6} {'beats':>6} "
          f"{'on-grid@1':>9} {'@2':>6} {'@3':>7} {'cover@2':>8} {'|err| med':>9}")
    for path in args[1:]:
        c = read_csv(path)
        d, corr = align(c, host)
        n = min(len(c["total"]), len(host["total"]) - d) if d >= 0 else min(len(c["total"]) + d, len(host["total"]))
        beats = c["beat"]
        beats = beats[d:d + n] if d >= 0 else beats[:n]
        per = np.median(c["period"][c["locked"] > 0]) if (c["locked"] > 0).any() else 0
        bi = np.nonzero(beats)[0]
        err = (bi - ph) % p
        err = np.where(err > p / 2, err - p, err)
        ae = np.abs(err)
        tot = len(range(ph, n, p))
        cov = sum(1 for f in range(ph, n, p) if len(bi) and np.min(np.abs(bi - f)) <= 2)
        print(f"{path.split('/')[-1]:<20} {corr:5.2f} {n:5d} {per:6.0f} {len(bi):6d} "
              f"{(np.mean(ae <= 1)*100 if len(ae) else 0):8.1f}% "
              f"{(np.mean(ae <= 2)*100 if len(ae) else 0):5.1f}% "
              f"{(np.mean(ae <= 3)*100 if len(ae) else 0):6.1f}% "
              f"{cov/max(1,tot)*100:7.1f}% "
              f"{(np.median(ae) if len(ae) else 0):9.1f}")


main()
