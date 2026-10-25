#!/usr/bin/env python3
"""Does each earring actually hear the speaker? Run this before any pair analysis.

Every conclusion about agreement assumes both earrings are measuring the same
sound. That assumption cost a session: one earring's smoothed spectrum had a
0.5-doubling range over 291 s of music and the *same* range over 40 s of silence,
while the other went 0.5 -> 2.9 doublings between the two. The unresponsive unit's
spectrum did move when the music started, by the same -0.3 doublings in every band,
and that uniformity is the tell: sound reaching two differently-placed units
changes band levels by *different* amounts, while the AGC moves all bands equally.

So the test is not "are the two spectra equal" - two working earrings are unequal,
and that difference is the object of study. It is whether each unit *responds*:

  pre-AGC rise  true (unnormalised) band energy when the sound starts, minus the
            same in silence, in doublings. This is the primary test, and it is the
            only one that cannot lie: the AGC holds the smoothed spectrum level, so
            a working earring can look perfectly flat there. Reading only the
            smoothed bands called a healthy unit deaf more than once.
  spread    p95-p5 of each band over a run, in doublings. A unit listening to music
            has several doublings of dynamic range; one listening to its own noise
            floor has a few tenths.
  response  spread gained by turning the sound on. Reported as context.
  shape     change in the mean band profile, after removing its mean. An all-band
            offset is the AGC and is ignored; what is left has to be band-selective.

Absolute level is reported as well, because a unit whose AGC sits at a high gain
while its input peak barely moves is attenuated rather than dead, which points at
the microphone, its bias, or the battery.

A unit that fails is usually sitting badly rather than broken: the unit that showed
no response over 291 s of one capture responded normally to a tone minutes later,
with nothing changed but where it lay. Re-run this after moving it before blaming
the hardware or the DSP.

  tools/hearing.py [--seconds 30] [--file TRACK] [--outdir DIR]

Exit status is 0 when both respond.
"""
import argparse
import os
import subprocess
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import specdump  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
NB = 8


def capture(outdir, seconds, file):
    os.makedirs(outdir, exist_ok=True)
    argv = [sys.executable, os.path.join(HERE, "specdump.py"),
            "--seconds", str(seconds), "--outdir", outdir]
    if file:
        argv += ["--file", file]
    subprocess.run(argv, check=True, stdout=subprocess.DEVNULL)


def phases(outdir):
    """Per-side (smoothed, raw, input peak, gain) from a capture by capture().

    Both spectra are returned because they answer different questions. `raw` is
    this frame's true band energy before any normalisation, so it is where an
    arriving sound actually shows up. `snap` is the smoothed spectrum the DSP
    selects patterns from, deliberately level-normalised: the AGC exists to remove
    exactly the change that says "sound started", so a unit that hears the speaker
    perfectly well can look flat here. Testing only `snap` calls a working earring
    deaf - which it did, repeatedly.
    """
    out = {}
    for name in ("L", "R"):
        d = specdump.load(os.path.join(outdir, f"spec-{name}.csv"))
        s = np.stack([d[f"snap{i}"] for i in range(NB)], axis=1)
        r = np.stack([d[f"raw{i}"] for i in range(NB)], axis=1)
        out[name] = (s, r, d["agcPeak"], d["agcShift"])
    return out


def spread(s):
    """p95-p5 per band, in doublings of band energy."""
    return (np.percentile(s, 95, axis=0) - np.percentile(s, 5, axis=0)) / 256.0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seconds", type=float, default=30)
    ap.add_argument("--file",
                    default="/home/max/Documents/led-earrings/podcast.mp3")
    ap.add_argument("--outdir", default="/tmp/hearing")
    a = ap.parse_args()

    print(f"phase 1: room only, {a.seconds:.0f} s", flush=True)
    quiet = os.path.join(a.outdir, "quiet")
    capture(quiet, a.seconds, None)
    print(f"phase 2: {os.path.basename(a.file)}, {a.seconds:.0f} s", flush=True)
    loud = os.path.join(a.outdir, "loud")
    capture(loud, a.seconds, a.file)

    q, l = phases(quiet), phases(loud)
    bad = []
    print("\n  unit       input peak p5/ p50/ p95  gain   spread  silence"
          "   sound  response")
    for name in ("L", "R"):
        sq, rq, pq, gq = q[name]
        sl, rl, pl, gl = l[name]
        s1, s2 = spread(sq), spread(sl)
        resp = s2.mean() - s1.mean()
        # The level the AGC had to absorb: true energy before vs after the sound
        # started. This is the honest "did sound arrive" measurement.
        rise = (np.median(rl, axis=0) - np.median(rq, axis=0)) / 256.0
        gain = float(np.median(rise))
        prof = (sl.mean(0) - sq.mean(0)) / 256.0
        shape = float(np.abs(prof - prof.mean()).mean())
        p5, p50, p95 = np.percentile(pl, [5, 50, 95])
        print(f"  {name:4} {p5:9.0f} {p50:8.0f} {p95:8.0f}   "
              f"{sorted(set(gl.astype(int).tolist()))}   "
              f"{s1.mean():8.2f} {s2.mean():8.2f} {resp:>+9.2f}"
              f"   pre-AGC level rise {gain:+.2f} dbl")
        print("       pre-AGC rise per band, doublings: "
              + " ".join(f"{v:+.2f}" for v in rise))
        print("       smoothed profile change (AGC-corrected): "
              + " ".join(f"{v:+.1f}" for v in prof)
              + f"   band-selective part {shape:.2f}")
        # A unit with a working microphone and a speaker in the room shows a
        # clear pre-AGC rise. Judge on that; the smoothed numbers are context.
        if gain < 0.7:
            bad.append(name)

    print()
    if bad:
        print(f"  NO SOUND ARRIVING at: {' and '.join(bad)}")
        print("  Its true (pre-AGC) band energy does not rise when the track starts,")
        print("  so this unit is not measuring the speaker, and any cross-device")
        print("  comparison measures that instead of the music. Check, in order:")
        print("  the unit is not muted/asleep, the speaker is the sink ffplay picks")
        print("  (wpctl status, default sink), sound actually leaves it, and only")
        print("  then the unit's placement.")
        return 1
    print("  both earrings respond to the speaker; pair analysis is meaningful.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
