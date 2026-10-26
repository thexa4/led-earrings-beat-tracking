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

  band rise     the raw band energies when the sound starts against the same in
            silence, as a ratio in doublings. This is the primary test, because the
            AGC holds the smoothed spectrum level, so a working earring can look
            perfectly flat there -- reading only the smoothed bands called a healthy
            unit deaf more than once.
            It is NOT pre-AGC, and used to be labelled that way. The raw column is
            bank.energy, which the bank scales by gainShift (bank.go, dx :=
            scalePow(x-b.hpX, b.gainShift)) before filtering, so the AGC's gain is
            already inside it. Comparing two phases is still sound while the gain
            holds still, which is what the gain column is there to check: if the
            shift differs between the quiet and loud phase, the ratio has to be
            corrected by the difference before it means anything.
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
    bank.energy: this frame's band magnitudes, summed linearly over the frame and
    carrying the AGC's gain, so it is where an arriving sound actually shows up --
    but as a LINEAR quantity, where `snap` is a Q8 logarithm. The two are not
    interchangeable and no arithmetic may be shared between them: subtracting and
    dividing by 256 turns a Q8 log2 difference into doublings and turns a linear
    difference into nothing at all.

    `snap` is the smoothed spectrum the DSP selects patterns from, deliberately
    level-normalised: the AGC exists to remove exactly the change that says "sound
    started", so a unit that hears the speaker perfectly well can look flat there.
    Testing only `snap` calls a working earring deaf - which it did, repeatedly.
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
        # Did the band energy actually rise when the sound started? raw is a
        # LINEAR accumulated magnitude, so the answer is a ratio, taken in log2.
        # This line used to read (median(rl) - median(rq)) / 256, which is the
        # Q8-log2 recipe applied to a linear quantity: it agreed with the truth
        # for ratios near 1 by coincidence and fell apart as soon as they were
        # not. On a measured capture it reported -19.98 doublings for a band whose
        # true ratio was 3900/9014 = -1.21, because 5114/256 happens to be 19.98.
        # The sign survived, which is why the mistake was never obvious: the
        # verdict this produced was right often enough to keep being trusted.
        lo, lq = np.maximum(np.median(rl, axis=0), 1.0), np.maximum(np.median(rq, axis=0), 1.0)
        rise = np.log2(lo / lq)
        # The bank scales every sample by gainShift before filtering (bank.go),
        # and energy sums magnitudes, so energy carries the gain: one bit of shift
        # is exactly one doubling here whether or not the room changed. A unit
        # whose gain moved between the phases therefore reports a rise it did not
        # hear, and an uncorrected comparison can invert the sign.
        dshift = float(np.median(gl) - np.median(gq))
        rise = rise - dshift
        gain = float(np.median(rise))
        if abs(dshift) >= 1:
            print(f"  {name:4} gain moved {dshift:+.0f} bits between phases; "
                  f"corrected out (raw ratio would have been "
                  f"{gain + dshift:+.2f} dbl)")
        prof = (sl.mean(0) - sq.mean(0)) / 256.0
        shape = float(np.abs(prof - prof.mean()).mean())
        p5, p50, p95 = np.percentile(pl, [5, 50, 95])
        print(f"  {name:4} {p5:9.0f} {p50:8.0f} {p95:8.0f}   "
              f"{sorted(set(gl.astype(int).tolist()))}   "
              f"{s1.mean():8.2f} {s2.mean():8.2f} {resp:>+9.2f}"
              f"   band energy rise {gain:+.2f} dbl")
        print("       band energy rise per band, doublings: "
              + " ".join(f"{v:+.2f}" for v in rise))
        print("       smoothed profile change (AGC-corrected): "
              + " ".join(f"{v:+.1f}" for v in prof)
              + f"   band-selective part {shape:.2f}")
        # A unit with a working microphone and a speaker in the room shows a
        # clear rise. Judge on that; the smoothed numbers are context.
        if gain < 0.7:
            bad.append(name)
            # Before believing a failure, rule out the one thing that fakes it.
            # These units inject their own LED refresh into the converter at up
            # to +16400 counts, one sample wide, which is larger than any room
            # signal and swamps the band energies it is meant to measure; see
            # tools/haltab.py, which separates it by halting the CPU. A unit can
            # be hearing perfectly and still fail this test because its own
            # output is the loudest thing in its window.
            print("       (before blaming this unit: run tools/haltab.py. Its own"
                  " LED refresh is often the loudest thing in the window.)")

    print()
    if bad:
        print(f"  NO SOUND ARRIVING at: {' and '.join(bad)}")
        print("  Its band energy does not rise when the track starts, so this unit")
        print("  is not measuring the speaker, and any cross-device comparison")
        print("  measures that instead of the music. But this test is easy to fail")
        print("  for reasons that are not the microphone, so check in order:")
        print("   1. tools/haltab.py --windows 20. These units inject their own LED")
        print("      refresh into the converter at up to +16400 counts, one sample")
        print("      wide, which is far larger than any room signal. An earring")
        print("      that is animating can be hearing the room perfectly and still")
        print("      show no rise, because its own output is the band energy.")
        print("   2. The unit is not muted or asleep.")
        print("   3. The speaker is the sink ffplay picks (wpctl status) and sound")
        print("      actually leaves it.")
        print("   4. Only then the unit's placement, then its hardware.")
        return 1
    print("  both earrings respond to the speaker; pair analysis is meaningful.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
