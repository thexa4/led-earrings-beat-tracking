#!/usr/bin/env python3
"""Live level meter on the ADC's own DMA window - for a hand-held acoustic test.

Everything else in tools/ analyses a recording, which means it can only answer a
question after the fact, and after the fact it is too late to find out whether
the sound source was actually pointed at the earrings. This prints the peak-to-
peak and RMS of the window the ADC is filling, a few times a second, so someone
can shake the board / snap fingers / move a speaker next to it and see the number
move.

  python3 tools/livelvl.py --elf firmware.elf --ports 4445 4446 --names L R

Reading the numbers: the codes are the device's own 16-bit oversampled ADC codes
(idle ~17300, so +-1 count is 1/16 of a 12-bit LSB). Room sound shows up as p-p in
the hundreds; electrical noise alone sits near the sample-to-sample noise.
"""

import argparse
import os
import sys
import threading
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem  # noqa: E402

HOST = os.environ.get("FLASH_HOST", "localhost")
WINDOW = 250  # syncWindowSize


def worker(port, base, out, stop, delay):
    ocd = telem.OpenOCD(HOST, port)
    while time.time() < stop:
        try:
            words = ocd.mdw(base, WINDOW // 2)
        except Exception as e:  # noqa: BLE001
            out.append((time.time(), None, str(e)))
            time.sleep(0.3)
            continue
        w = np.array(words, dtype=np.uint32)
        x = np.empty(WINDOW, dtype=np.uint16)
        x[0::2] = (w & 0xFFFF).astype(np.uint16)
        x[1::2] = (w >> 16).astype(np.uint16)
        x = x.astype(np.float64)
        out.append((time.time(), (x.max() - x.min(), float(np.std(x)),
                                  float(np.mean(x))), None))
        time.sleep(delay)
    ocd.close()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--elf", default="firmware.elf")
    ap.add_argument("--ports", type=int, nargs="+", default=[4445, 4446])
    ap.add_argument("--names", nargs="+", default=None)
    ap.add_argument("--seconds", type=float, default=60)
    ap.add_argument("--delay", type=float, default=0.25,
                    help="pause between polls; the SWD link is the bottleneck")
    a = ap.parse_args()
    names = a.names or [f"p{p}" for p in a.ports]
    base = telem.resolve_symbols(a.elf)["main.adcData"]
    print(f"adcData = 0x{base:08x}; printing p-p / rms / mean of the 250-sample "
          f"window ({WINDOW / 16:.1f} ms) every {a.delay * 1000:.0f} ms")

    data = {n: [] for n in names}
    t0 = time.time()
    th = [threading.Thread(target=worker, args=(p, base, data[n], t0 + a.seconds, a.delay))
          for n, p in zip(names, a.ports)]
    for t in th:
        t.start()
    for t in th:
        t.join()

    print(f"\n{'t':>7} " + "  ".join(f"{n + ' p-p':>9}{n + ' rms':>9}" for n in names))
    ref = [r for r in data[names[0]] if r[1]]
    for i, (ts, v, err) in enumerate(ref):
        line = f"{ts - t0:7.2f} "
        for n in names:
            if i < len(data[n]) and data[n][i][1]:
                pp, rms, mean = data[n][i][1]
                line += f" {pp:9.0f}{rms:9.1f}"
            else:
                line += "         -"
        print(line + ("  " + (err or "") if err else ""))
    for n in names:
        v = [r[1] for r in data[n] if r[1]]
        if v:
            pp = np.array([q[0] for q in v])
            print(f"\n{n}: p-p min={pp.min():.0f} median={np.median(pp):.0f} "
                  f"max={pp.max():.0f}  ({len(v)} polls)")


main()
