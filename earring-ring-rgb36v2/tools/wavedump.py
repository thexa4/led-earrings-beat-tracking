#!/usr/bin/env python3
"""Print a frozen ADC window as a waveform, to see what the disturbance looks like.

The statistics tools answer "how big" and "how often". They cannot answer "what
shape", and shape is what identifies a source: a burst at one offset once per
window is the LED bit-bang; a full-scale square wave is a drive line; a
smooth many-cycle sinusoid at a fixed rate is a switching regulator; broadband
single-sample steps are the ADC's own kickback.

TIM22 is stopped around the read, so the window is one coherent 15.6 ms capture
(see freeze.py for why the read must be done that way at all), and both devices
are frozen and released together so the two traces are comparable.

  FLASH_HOST=localhost python3 tools/wavedump.py --outdir /tmp/psync/wave

Writes OUT/wave-<dev>.csv (one window per row) and prints the first few windows
as a plot. --tone also reports the coherent amplitude at that frequency, which
is the one number that says whether the room is audible at all underneath
whatever else is in the window.
"""
import argparse
import csv
import os
import struct
import sys
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem  # noqa: E402

TIM22 = 0x40011400
TIM_CR1 = TIM22 + 0x00
CEN = 1
N = 250


def window(o, base):
    return struct.unpack("<%dH" % N, struct.pack("<%dI" % (N // 2),
                                                 *o.mdw(base, N // 2)))


def plot(name, x):
    """ASCII plot of one window: 250 samples, ~40 rows, one char per sample."""
    lo, hi = int(min(x)), int(max(x))
    rows, cols = 26, 250
    grid = [[" "] * cols for _ in range(rows)]
    med = int(np.median(x))
    for i, v in enumerate(x):
        r = int((hi - v) * (rows - 1) / max(hi - lo, 1))
        grid[r][i] = "#"
    z = int((hi - med) * (rows - 1) / max(hi - lo, 1))
    for i in range(cols):
        if grid[z][i] == " ":
            grid[z][i] = "-"
    print(f"--- {name}: p-p {hi - lo}  min {lo}  max {hi}  median {med}")
    for r, row in enumerate(grid):
        lab = f"{hi - r * (hi - lo) / (rows - 1):7.0f}" if r % 4 == 0 else " " * 7
        print(lab + "|" + "".join(row))
    print(" " * 7 + "+" + "-" * 24 + "+" + "-" * 24 + "+" + "-" * 24 + "+" + "-" * 20)
    print(" " * 8 + "0" + " " * 55 + "64" + " " * 55 + "125" + " " * 53 + "187")
    d = np.array(x, dtype=np.float64) - med
    e = max(np.dot(d, d), 1.0)
    print("  autocorr lag1..12:",
          " ".join(f"{np.dot(d[:-k], d[k:]) / e:+.2f}" for k in range(1, 13)))
    f = np.abs(np.fft.rfft(d * np.hanning(len(d))))
    k = np.argsort(f)[::-1][:6]
    print("  top tones (Hz):",
          " ".join(f"{i * 16000 / (2 * len(d)):.0f}:{f[i] / f[0]:.2f}" for i in k if i) or "-")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default=os.environ.get("FLASH_HOST", "pi"))
    ap.add_argument("--elf", default="firmware.elf")
    ap.add_argument("--outdir", default="/tmp/psync/wave")
    ap.add_argument("--windows", type=int, default=12)
    ap.add_argument("--tone", type=float, default=0.0,
                    help="also report coherent amplitude at this frequency")
    ap.add_argument("--plot", type=int, default=2, help="windows to print per device")
    a = ap.parse_args()
    os.makedirs(a.outdir, exist_ok=True)
    base = telem.resolve_symbols(a.elf)["main.adcData"]
    devs = [(name, telem.OpenOCD(a.host, port)) for port, name in ((4445, "L"), (4446, "R"))]

    # The file handle is kept next to the writer rather than dropped. Writing
    # through a csv.writer whose file is never closed leaves whatever is still in
    # the buffer unwritten, so a short run could silently lose its whole capture
    # while a long one looked fine purely because it overflowed the buffer. The
    # crash on exit (csv.writer has no close) was the visible half of that.
    files, fw = {}, {}
    for name, _ in devs:
        fh = open(f"{a.outdir}/wave-{name}.csv", "w", newline="")
        files[name], fw[name] = fh, csv.writer(fh)
    for name, o in devs:
        fw[name].writerow([f"s{i}" for i in range(N)])

    for w in range(a.windows):
        for name, o in devs:
            try:
                o.cmd(f"mww 0x{TIM_CR1:08x} 0")   # stop the trigger
                x = window(o, base)
                o.cmd(f"mww 0x{TIM_CR1:08x} {CEN}")
            except Exception as e:  # noqa: BLE001
                print(f"{name} read failed: {e}")
                continue
            fw[name].writerow(list(x))
            if w < a.plot:
                plot(f"{name} w{w}", np.array(x))
            if a.tone:
                d = np.array(x, dtype=np.float64) - np.median(x)
                t = np.arange(N) / 16000.0
                c = 2 * np.dot(d * np.hanning(N), np.exp(-2j * np.pi * a.tone * t)) / N
                print(f"  {name} coherent @{a.tone:.0f} Hz: {abs(c):8.1f} counts"
                      f"  (window p-p {max(x) - min(x)})")
        time.sleep(0.4)
    for name, o in devs:
        files[name].flush()
        files[name].close()
        o.close()


main()
