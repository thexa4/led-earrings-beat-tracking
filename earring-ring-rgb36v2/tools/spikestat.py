#!/usr/bin/env python3
"""How much of each device's signal energy is its own transients?

The pattern the earrings show is the argument of the maximum of the band
deviations, and deviations are fitted from the band energies, which are sums of
squares over the analysis window. A single large sample therefore contributes to
every band at once, in proportion to nothing at all - an impulse has a flat
spectrum - and if the impulses carry a comparable energy to the music they do not
add a hiss on top of the analysis, they REPLACE it.

That is what this measures, because it is what the bench showed and the host model
did not. On the rig (tools/clip.py, tools/livelvl.py, same music, same level):

    device L   idle p-p 13-33    music p-p median 221
    device R   idle p-p 114-134  music p-p median 1044, peaks 16455

with zero samples against either rail, so the peaks are not clipping. R's peaks are
single-sample excursions, and the pipeline running on R today - the flashed build,
before the median filter in Pipeline.feed existed - reports a spectrum with no
structure at all: decoding the shape field out of its telemetry hash, L's bands sit
0.85/0.89 codes above the fitted line where R's sit at 0.01 across every band. R
is not hearing flat music, it is analysing its own clicks.

Per frozen 250-sample window this prints:

    mad     median absolute deviation from the median: the body of the signal
    pp      peak-to-peak
    k1      number of samples more than K mad from the median
    share   those samples' share of the window's total energy  <-- the number
    step    largest single-sample step

`share` is the number to compare against the host model. If it is near zero the
window's spectrum is the music's; if it is near one the spectrum belongs to the
transients and any decision made from it is meaningless.

  FLASH_HOST=localhost python3 tools/spikestat.py --seconds 30
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


def window(o, base):
    return struct.unpack("<%dH" % N, struct.pack("<%dI" % (N // 2),
                                                 *o.mdw(base, N // 2)))


def analyse(s, k):
    """(mad, pp, outliers, energy share of outliers, largest step)."""
    med = sorted(s)[len(s) // 2]
    d = sorted(abs(v - med) for v in s)
    mad = max(d[len(d) // 2], 1)
    thr = k * mad
    base = sum((v - med) ** 2 for v in s)
    out = [v for v in s if abs(v - med) > thr]
    share = sum((v - med) ** 2 for v in out) / base if base else 0.0
    step = max(abs(s[i] - s[i - 1]) for i in range(1, len(s)))
    return mad, max(s) - min(s), len(out), share, step


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default=os.environ.get("FLASH_HOST", "localhost"))
    ap.add_argument("--seconds", type=float, default=20.0)
    ap.add_argument("--k", type=float, default=6.0,
                    help="outlier threshold, in MADs")
    ap.add_argument("--quiet", action="store_true")
    a = ap.parse_args()
    base = telem.resolve_symbols("firmware.elf")["main.adcData"]
    devs = [(name, telem.OpenOCD(a.host, port))
            for port, name in ((4445, "L"), (4446, "R"))]
    got = {n: [] for n, _ in devs}
    if not a.quiet:
        print(f"{'t':>6s} {'dev':3s} {'mad':>6s} {'pp':>7s} {'k':>4s} "
              f"{'share':>7s} {'step':>7s}")
    t_end = time.time() + a.seconds
    while time.time() < t_end:
        for name, o in devs:
            cr1 = o.mdw(TIM_CR1, 1)[0]
            o.cmd(f"mww 0x{TIM_CR1:08x} {cr1 & ~1}")
            time.sleep(0.01)
            s = window(o, base)
            o.cmd(f"mww 0x{TIM_CR1:08x} {cr1 | 1}")
            r = analyse(s, a.k)
            got[name].append(r)
            if not a.quiet:
                print(f"{time.time()-t_end+a.seconds:6.1f} {name:3s} {r[0]:6d} "
                      f"{r[1]:7d} {r[2]:4d} {r[3]:7.3f} {r[4]:7d}")
            time.sleep(0.05)
    for name, _ in devs:
        v = got[name]
        if not v:
            continue
        q = lambda i: sorted(x[i] for x in v)  # noqa: E731
        mad, pp, k, sh, st = (q(i) for i in range(5))
        n = len(v)
        pick = lambda arr, p: arr[int(p * (len(arr) - 1))]  # noqa: E731
        print(f"\n{name} ({n} windows, K={a.k:g}): mad med {pick(mad,.5):.0f} "
              f"p90 {pick(mad,.9):.0f} | pp med {pick(pp,.5):.0f} "
              f"p90 {pick(pp,.9):.0f} max {pp[-1]:.0f} | "
              f"outliers med {pick(k,.5):.0f} p90 {pick(k,.9):.0f} | "
              f"share med {pick(sh,.5):.3f} p90 {pick(sh,.9):.3f} "
              f"max {sh[-1]:.3f} | step med {pick(st,.5):.0f} max {st[-1]:.0f}")
    for _, o in devs:
        o.close()


main()
