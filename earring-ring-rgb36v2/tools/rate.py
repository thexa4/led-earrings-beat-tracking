#!/usr/bin/env python3
"""How fast does the ADC actually deliver samples? Measured, not inferred.

Almost every analysis in tools/ assumes the ring fills at 16 kSPS, because that is
what TIM22 is configured for. But the ADC only services a trigger if it finished
the previous one, and with 32x oversampling a sample takes
OSR x (SMP + 10.5) ADC cycles: if that exceeds the trigger period, the ADC
silently drops triggers and the real rate is a sub-multiple of the configured
one. Every spectrum, filter and beat period would then be wrong by the same
factor - which is exactly the kind of error that makes a microphone look dead.

The DMA's remaining-transfer counter (CNDTR) is the truth: it counts down once
per sample, circularly. Polling it faster than one wrap and unwrapping the deltas
gives the rate. The SWD link is the limit - one mdw is ~1-3 ms, so this resolves
a 250-sample wrap period comfortably but nothing slower to worry about: a wrap
missed in the polling shows up as a suspiciously round low estimate, and the
median-vs-mean columns make that visible.

  python3 tools/rate.py --ports 4445 4446 --names L R
"""

import argparse
import os
import statistics
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem  # noqa: E402

HOST = os.environ.get("FLASH_HOST", "localhost")
DMA_CNDTR = 0x4002000C
RING = 250  # syncWindowSize: DMA circular transfer count


def measure(port, seconds, n=400):
    ocd = telem.OpenOCD(HOST, port)
    prev = ocd.mdw(DMA_CNDTR, 1)[0]
    tprev = time.perf_counter()
    t_end = tprev + seconds
    rates, gaps = [], []
    while len(rates) < n and time.perf_counter() < t_end:
        try:
            cur = ocd.mdw(DMA_CNDTR, 1)[0]
        except Exception:  # noqa: BLE001
            break
        t = time.perf_counter()
        dt = t - tprev
        d = (prev - cur) % RING
        if dt > 0:
            rates.append(d / dt)
            gaps.append(dt)
        prev, tprev = cur, t
    ocd.close()
    return rates, gaps


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ports", type=int, nargs="+", default=[4445, 4446])
    ap.add_argument("--names", nargs="+", default=None)
    ap.add_argument("--seconds", type=float, default=6.0)
    a = ap.parse_args()
    names = a.names or [f"p{p}" for p in a.ports]
    print("sample rate from the DMA remaining-transfer counter "
          f"(ring {RING}, wrap must be < every poll)")
    for name, port in zip(names, a.ports):
        rates, gaps = measure(port, a.seconds)
        if not rates:
            print(f"{name}: no reads")
            continue
        r = statistics.median(rates)
        print(f"{name:>6}: median={r:8.0f} SPS  mean={statistics.mean(rates):8.0f}"
              f"  p10={sorted(rates)[len(rates) // 10]:8.0f}"
              f"  poll gap median={statistics.median(gaps) * 1e3:.1f} ms"
              f"  max={max(gaps) * 1e3:.0f} ms")
        # A dropped-trigger sub-multiple of 16 k would land on these.
        for k, label in [(1, "16000 (no drops)"), (2, "8000 (every 2nd)"),
                         (3, "5333 (every 3rd)"), (4, "4000 (every 4th)")]:
            if abs(r - 16000 / k) < 0.06 * r:
                print(f"        ~ {label}")
        if max(gaps) * 16000 > RING:
            print(f"        note: some polls were {max(gaps) * 1e3:.0f} ms apart, "
                  f"> one {RING}-sample wrap; mean is a lower bound")


main()
