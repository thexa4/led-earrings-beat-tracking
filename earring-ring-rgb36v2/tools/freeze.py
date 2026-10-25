#!/usr/bin/env python3
"""Is the impulse noise real, or is it the read?

Every tool here reads the 250-sample DMA window over OpenOCD's telnet, which
takes 7-15 ms at 17 kB/s - during which the DMA writes 112-240 more samples into
the same buffer. So a "window" assembled that way is not one window: wherever
the ring wrapped under the read, the sequence jumps, and a jump looks exactly
like the single-sample, +-1000-count, device-uncorrelated impulse that catch.py
reported and that this file is here to test.

Freezing the trigger removes the doubt. TIM22's CEN bit stops the trigger, so
no new conversion is requested and the ring stops moving; the ADC, its DMA
channel and the firmware's counters are left in whatever state they were in, and
setting CEN again resumes the stream with no overrun and no re-initialisation.
(Stopping the DMA channel instead was tried first and does not come back: the
ADC loses its request stream, ends up with ADSTART cleared, and the ring stays
frozen at a CNDTR that never moves again - which is how both bench devices had
to be reset by hand after this tool's first run.)

Compared here, per device:

  frozen  statistics of the coherent window
  running the same statistics on the same buffer read without stopping the DMA

If the impulse rate collapses in the frozen column, the impulses were the
measurement, and anything built on them - including "the AGC peak is set by LED
noise" - has to be re-examined.

  FLASH_HOST=localhost python3 tools/freeze.py --seconds 60
"""
import argparse
import os
import socket
import struct
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem  # noqa: E402

TIM22 = 0x40011400
TIM_CR1 = TIM22 + 0x00
TIM22_CEN = 1
DMA1 = 0x40020000
DMA_CNDTR1 = DMA1 + 0x0C
N = 250


def rd(o, a):
    return o.mdw(a, 1)[0]


def wr(o, a, v):
    o.cmd(f"mww 0x{a:08x} {v:#x}")


def window(o, base):
    """The 250-sample ring as uint16 samples.

    Unpack through uint32: mdw hands back 32-bit words and a sample is 16 bits,
    so the two per word go in the order memory has them.
    """
    return struct.unpack("<%dH" % N, struct.pack("<%dI" % (N // 2),
                                                 *o.mdw(base, N // 2)))


def stats(buf):
    """mean, MAD and the count of samples more than 8 MAD from the mean.

    Takes samples, not bytes. Passing the packed buffer here was the bug that
    made the first run of this tool look like a discovery: summing bytes gives a
    "mean level" of 75-96 and a "p-p" of 30-170, which is the shape of a byte
    histogram, not of a 17000-count ADC idle level.
    """
    s = sorted(buf)
    mean = sum(buf) // len(buf)
    d = sorted(abs(v - mean) for v in buf)
    mad = d[len(d) // 2]
    thr = max(8 * mad, 8)
    spk = sum(1 for v in buf if abs(v - mean) > thr)
    pp = s[-1] - s[0]
    # Largest single-sample step anywhere in the window: what the pipeline's
    # differentiator and the AGC peak actually see.
    step = max(abs(buf[i] - buf[i - 1]) for i in range(1, len(buf)))
    return mean, mad, pp, spk, step


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default=os.environ.get("FLASH_HOST", "pi"))
    ap.add_argument("--seconds", type=float, default=60.0)
    a = ap.parse_args()
    syms = telem.resolve_symbols("firmware.elf")
    base = syms["main.adcData"]
    # One connection per target: OpenOCD's telnet server serves a single client,
    # so the same handle does the freeze, the read and the restart.
    out = [(name, telem.OpenOCD(a.host, port))
           for port, name in ((4445, "L"), (4446, "R"))]
    t_end = time.time() + a.seconds
    print(f"{'t':>6s} {'dev':3s}   mean    mad     pp  spike  step | "
          "mean    mad     pp  spike  step")
    print(" " * 7 + "frozen (DMA stopped)                 "
          "running (read while written)")
    # Where in the window do the outliers sit? Random noise puts them anywhere;
    # a fixed position means something in the firmware is kicking the input, and
    # the period of the pattern says what - the LED refresh is the candidate,
    # since the bit-bang runs once per animation frame (~16 Hz = ~1000 samples,
    # far longer than this window, so a refresh would show as a burst at one
    # offset rather than a spread).
    hist = {name: [0] * N for name, _ in out}
    while time.time() < t_end:
        for name, o in out:
            cr1 = rd(o, TIM_CR1)
            wr(o, TIM_CR1, cr1 & ~TIM22_CEN)
            time.sleep(0.01)
            frozen = window(o, base)
            wr(o, TIM_CR1, cr1 | TIM22_CEN)
            time.sleep(0.02)
            run = window(o, base)
            f = stats(frozen)
            r = stats(run)
            mean, mad = f[0], f[1]
            thr = max(8 * mad, 8)
            for i, v in enumerate(frozen):
                if abs(v - mean) > thr:
                    hist[name][i] += 1
            print(f"{time.time()-t_end+a.seconds:6.1f} {name:3s} "
                  + " ".join(f"{v:6d}" for v in f) + " | "
                  + " ".join(f"{v:6d}" for v in r), flush=True)
        time.sleep(0.5)
    for name, _ in out:
        h = hist[name]
        tot = sum(h)
        print(f"\n{name}: {tot} outlier samples, position histogram "
              f"(one '#' per hit, 250 samples = 15.6 ms)")
        if tot:
            peak = max(h)
            rows = 4
            for rr in range(rows):
                lo = peak * (rows - 1 - rr) // rows
                hi = peak * (rows - rr) // rows
                print("   " + "".join(
                    "#" if lo < v <= hi else ("." if v else " ")
                    for v in h))
            # Coarse structure: hits per 10-sample bucket.
            b = [sum(h[i:i + 10]) for i in range(0, N, 10)]
            print("   per 10 samples:", " ".join(f"{v:2d}" for v in b))

    # A run that dies between the freeze and the thaw leaves the trigger off,
    # so make sure it is back on for whatever tool runs next.
    for name, o in out:
        wr(o, TIM_CR1, rd(o, TIM_CR1) | TIM22_CEN)
    for name, o in out:
        cnd = [rd(o, DMA_CNDTR1) for _ in range(2)]
        print(f"{name}: DMA CNDTR {cnd[0]} -> {cnd[1]} "
              f"({'advancing, capture restored' if cnd[0] != cnd[1] else 'STOPPED'})")
        o.close()


main()
