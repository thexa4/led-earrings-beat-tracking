#!/usr/bin/env python3
"""Does the microphone's signal hit the converter's rails?

The AGC in syncdsp only ever AMPLIFIES: agcShift runs from 0 (no gain) up to
AgcMaxShift, chosen so the tracked input peak lands inside [AgcLowPeak,
AgcHighPeak]. That is the right shape for a device whose worst case is a quiet
room, and the wrong shape for one whose worst case is a loud one, because
clipping happens in the converter, before any of the pipeline's arithmetic, and
no amount of subsequent gain division puts the waveform back. A clipped cycle is
a square-ish cycle: its harmonics fill every band, which flattens the spectrum
the pattern is read from.

This matters because the two bench devices are not the same. tools/livelvl.py
measured, on the same music at the same level:

    device L   p-p median 221   p90 4179   max 4299
    device R   p-p median 1044  p90 8394   max 16575   <- full scale

16575 on a 16-bit oversampled converter is the entire range, and the microphone
biases at 0.26 of full scale (tools/freeze.py: idle ~17120), so the downward
excursion has only 17120 codes to travel before it hits zero. A 16575-count
peak-to-peak swing centred there is guaranteed to rail.

Freezes the trigger around each read, exactly as freeze.py does, so the window is
one coherent 15.6 ms capture rather than a splice, and reports how much of it sits
against a rail.

  FLASH_HOST=localhost python3 tools/clip.py --seconds 60
  FLASH_HOST=localhost python3 tools/clip.py --seconds 40   # while audio plays

Read the "rail%" column: that is the fraction of samples within RAIL margin codes
of 0 or 65535. Anything above about 1% is audible distortion in the pipeline's
input, and the pattern it derives is the distortion's spectrum, not the music's.
"""
import argparse
import os
import struct
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem  # noqa: E402

TIM_CR1 = 0x40011400
N = 250
RAIL = 4  # codes from the end of the range to count as "against the rail"


def window(o, base):
    return struct.unpack("<%dH" % N, struct.pack("<%dI" % (N // 2),
                                                 *o.mdw(base, N // 2)))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default=os.environ.get("FLASH_HOST", "localhost"))
    ap.add_argument("--seconds", type=float, default=30.0)
    ap.add_argument("--quiet", action="store_true", help="summary only")
    a = ap.parse_args()
    syms = telem.resolve_symbols("firmware.elf")
    base = syms["main.adcData"]
    devs = [(name, telem.OpenOCD(a.host, port))
            for port, name in ((4445, "L"), (4446, "R"))]
    agg = {name: [] for name, _ in devs}
    t_end = time.time() + a.seconds
    if not a.quiet:
        print(f"{'t':>6s} {'dev':3s} {'min':>6s} {'max':>6s} {'mean':>7s} "
              f"{'pp':>6s} {'rail%':>6s} {'lo':>4s} {'hi':>4s}")
    while time.time() < t_end:
        for name, o in devs:
            cr1 = o.mdw(TIM_CR1, 1)[0]
            o.cmd(f"mww 0x{TIM_CR1:08x} {cr1 & ~1}")
            time.sleep(0.01)
            s = window(o, base)
            o.cmd(f"mww 0x{TIM_CR1:08x} {cr1 | 1}")
            lo = sum(1 for v in s if v <= RAIL)
            hi = sum(1 for v in s if v >= 65535 - RAIL)
            mean = sum(s) / len(s)
            rec = (min(s), max(s), mean, max(s) - min(s),
                   100 * (lo + hi) / len(s), lo, hi)
            agg[name].append(rec)
            if not a.quiet:
                print(f"{time.time() - t_end + a.seconds:6.1f} {name:3s} "
                      f"{rec[0]:6d} {rec[1]:6d} {rec[2]:7.0f} {rec[3]:6d} "
                      f"{rec[4]:6.2f} {rec[5]:4d} {rec[6]:4d}")
            time.sleep(0.05)
    for name, _ in devs:
        v = agg[name]
        if not v:
            continue
        pp = sorted(r[3] for r in v)
        rail = sorted(r[4] for r in v)
        print(f"\n{name}: {len(v)} windows  p-p med {pp[len(pp)//2]:.0f} "
              f"p90 {pp[int(0.9*len(pp))]:.0f} max {pp[-1]:.0f}   "
              f"rail% med {rail[len(rail)//2]:.2f} "
              f"p90 {rail[int(0.9*len(rail))]:.2f} max {rail[-1]:.2f}   "
              f"windows with any rail sample {100*sum(1 for r in v if r[4]>0)/len(v):.0f}%")
    for _, o in devs:
        o.close()


main()
