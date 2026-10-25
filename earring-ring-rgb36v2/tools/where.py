#!/usr/bin/env python3
"""Are device R's impulses periodic? Fixed spacing means electrical, not acoustic.

spikestat.py measured the size of the problem: on the right-hand earring, single
sample excursions carry 90% of the input energy with nothing playing, and 81% of it
while music is playing - where the left-hand earring measures 0% and 45%. Since the
band energies the hash and the pattern are built from are sums of squares over the
window, that means R analyses its own clicks rather than the room: an impulse has a
flat spectrum, which is exactly what R's telemetry shows (every band's deviation
from the fitted spectral line reads 0.00 shape codes, against L's 0.85).

The fix depends entirely on what makes them, so this finds the spacing.

Each frozen 250-sample window is read with TIM22 stopped, so it is one coherent
capture and an offset inside it means something. For every window, the samples more
than K MADs from the window median are located, and three things reported:

  offset    histogram of where inside the window they land. A spike at one offset
            means the firmware kicks the input once per window (LED bit-bang, a
            register write, the ADC's own sample-and-hold). Flat means they arrive
            at random times, which means acoustic or thermal.
  spacing   the gaps between consecutive impulses in the stream. A single peak at
            N samples is a frequency: 16000/N Hz.
  gap-mod   the spacing folded by small moduli, which finds a period hidden in
            amplitude noise.

  FLASH_HOST=localhost python3 tools/where.py --seconds 20
"""
import argparse
import os
import struct
import sys
import time
from collections import Counter

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem  # noqa: E402

TIM_CR1 = 0x40011400
N = 250


def window(o, base):
    return struct.unpack("<%dH" % N, struct.pack("<%dI" % (N // 2),
                                                 *o.mdw(base, N // 2)))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default=os.environ.get("FLASH_HOST", "localhost"))
    ap.add_argument("--seconds", type=float, default=20.0)
    ap.add_argument("--k", type=float, default=5.0)
    a = ap.parse_args()
    base = telem.resolve_symbols("firmware.elf")["main.adcData"]
    devs = [(name, telem.OpenOCD(a.host, port))
            for port, name in ((4445, "L"), (4446, "R"))]
    pos = {n: Counter() for n, _ in devs}
    gaps = {n: Counter() for n, _ in devs}
    nwin = Counter()
    t_end = time.time() + a.seconds
    while time.time() < t_end:
        for name, o in devs:
            cr1 = o.mdw(TIM_CR1, 1)[0]
            o.cmd(f"mww 0x{TIM_CR1:08x} {cr1 & ~1}")
            time.sleep(0.01)
            s = window(o, base)
            o.cmd(f"mww 0x{TIM_CR1:08x} {cr1 | 1}")
            med = sorted(s)[N // 2]
            d = sorted(abs(v - med) for v in s)
            mad = max(d[len(d) // 2], 1)
            idx = [i for i, v in enumerate(s) if abs(v - med) > a.k * mad]
            nwin[name] += 1
            for i in idx:
                pos[name][i] += 1
            for p, q in zip(idx, idx[1:]):
                gaps[name][q - p] += 1
            time.sleep(0.05)

    for name, _ in devs:
        n = nwin[name]
        tot = sum(pos[name].values())
        print(f"\n=== {name}: {tot} outlier samples in {n} windows "
              f"({100*tot/max(n*N,1):.1f}% of samples)")
        if not tot:
            continue
        top = pos[name].most_common(8)
        exp = tot / N
        print("  hottest offsets (expected if random: "
              f"{exp:.2f} each): " +
              " ".join(f"{i}:{c}={c/exp:.1f}x" for i, c in top))
        g = gaps[name].most_common(10)
        print("  commonest gaps: " + " ".join(f"{k}:{v}" for k, v in g))
        for m in (2, 3, 4, 5, 8, 10, 16, 20, 25, 40, 50):
            c = Counter()
            for k, v in gaps[name].items():
                c[k % m] += v
            srt = sorted(c.values())
            peak = srt[-1] / (sum(c.values()) / m) if sum(c.values()) else 0
            if peak > 1.6:
                print(f"  gap mod {m}: peak {peak:.2f}x over uniform "
                      f"(residual {max(c, key=c.get)})")
    for _, o in devs:
        o.close()


main()
