#!/usr/bin/env python3
"""Catch the big windows and look at them, rather than averaging them away.

Every statistic so far has been a mean over windows, and the means say "nothing
is there" - but the raw p-p trace is bimodal: most windows sit at tens of counts
and a minority jump past a thousand, which is 128 12-bit LSB, about a hundred
millivolts. Either that is sound (in which case the microphone works and every
"dead mic" conclusion was an averaging artefact), or it is the animation driving
current through the shared supply (in which case it is an artefact the DSP should
reject), or it is a read artefact. Those three look completely different
sample by sample, so this prints the samples.

  python3 tools/catch.py --ports 4445 4446 --names L R --trigger 400

Writes one CSV per device (every poll: time, p-p, rms) plus a text dump of the
first few windows that exceeded the trigger.
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
WINDOW = 250  # syncWindowSize: one DMA ring = 15.6 ms at 16 kSPS
FS = 16000.0


def worker(port, base, out, trig, dumps, stop):
    ocd = telem.OpenOCD(HOST, port)
    while time.time() < stop:
        try:
            words = ocd.mdw(base, WINDOW // 2)
        except Exception as e:  # noqa: BLE001
            out.append((time.time(), None, None, None))
            time.sleep(0.2)
            continue
        w = np.array(words, dtype=np.uint32)
        x = np.empty(WINDOW, dtype=np.uint16)
        x[0::2] = (w & 0xFFFF).astype(np.uint16)
        x[1::2] = (w >> 16).astype(np.uint16)
        x = x.astype(np.float64)
        t = time.time()
        pp = x.max() - x.min()
        out.append((t, pp, float(np.std(x)), x.mean()))
        if pp >= trig and len(dumps) < 4:
            dumps.append((t, x.copy()))
    ocd.close()


def plot(x, label):
    """A crude one-screen waveform: min/max per 5-sample column."""
    n = len(x) // 5 * 5
    v = x[:n].reshape(-1, 5)
    lo, hi = v.min(axis=1), v.max(axis=1)
    a, b = x.min(), x.max()
    rng = max(b - a, 1)
    rows = 17
    grid = [[" "] * len(lo) for _ in range(rows)]
    for c in range(len(lo)):
        r0 = int((hi[c] - a) / rng * (rows - 1))
        r1 = int((lo[c] - a) / rng * (rows - 1))
        for r in range(r1, r0 + 1):
            grid[rows - 1 - r][c] = "|"
    print(f"  {label}: {a:.0f} .. {b:.0f}, 250 samples = {WINDOW / FS * 1e3:.1f} ms")
    for r, row in enumerate(grid):
        val = b - (b - a) * r / (rows - 1)
        print(f"  {val:7.0f} " + "".join(row))


def spectrum(x, label):
    x = x - x.mean()
    f = np.abs(np.fft.rfft(x * np.hanning(len(x))))
    fr = np.fft.rfftfreq(len(x), 1 / FS)
    top = np.argsort(f)[-6:][::-1]
    print(f"  {label}: strongest components "
          + ", ".join(f"{fr[i]:.0f} Hz ({f[i]:.0f})" for i in top))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ports", type=int, nargs="+", default=[4445, 4446])
    ap.add_argument("--names", nargs="+", default=None)
    ap.add_argument("--elf", default="firmware.elf")
    ap.add_argument("--seconds", type=float, default=25)
    ap.add_argument("--trigger", type=float, default=400)
    ap.add_argument("--outdir", default="/tmp/psync/catch")
    a = ap.parse_args()
    os.makedirs(a.outdir, exist_ok=True)
    names = a.names or [f"p{p}" for p in a.ports]
    base = telem.resolve_symbols(a.elf)["main.adcData"]

    data, dumps = {}, {}
    t0 = time.time()
    th = []
    for n, p in zip(names, a.ports):
        data[n], dumps[n] = [], []
        t = threading.Thread(target=worker, args=(p, base, data[n], a.trigger,
                                                  dumps[n], t0 + a.seconds))
        t.start()
        th.append(t)
    for t in th:
        t.join()

    for n in names:
        rows = [r for r in data[n] if r[1] is not None]
        if not rows:
            print(f"{n}: nothing")
            continue
        pp = np.array([r[1] for r in rows])
        ts = np.array([r[0] for r in rows]) - t0
        np.savetxt(f"{a.outdir}/{n}.csv", np.c_[ts, pp], delimiter=",",
                   header="t,pp", comments="")
        big = ts[pp >= a.trigger]
        print(f"\n{n}: {len(rows)} polls, p-p median={np.median(pp):.0f} "
              f"max={pp.max():.0f}; {len(big)} polls >= {a.trigger:.0f}")
        if len(big):
            print("  big-poll times (s): " + " ".join(f"{v:.2f}" for v in big[:40]))
        for i, (t, x) in enumerate(dumps[n]):
            plot(x, f"{n} #{i} at t={t - t0:.2f}")
            spectrum(x, f"{n} #{i}")
    # Do the two devices see them at the same time?
    if len(names) == 2 and all(len(d[n]) for n, d in ((n, data) for n in names)):
        l = [(r[0], r[1]) for r in data[names[0]] if r[1] is not None]
        r = [(r[0], r[1]) for r in data[names[1]] if r[1] is not None]
        lb = [t for t, v in l if v >= a.trigger]
        rb = [t for t, v in r if v >= a.trigger]
        near = sum(1 for t in lb if any(abs(t - u) < 0.02 for u in rb))
        print(f"\ncoincidence: {near} of {len(lb)} {names[0]} events have a "
              f"{names[1]} event within 20 ms "
              f"(chance would be ~{len(lb) * len(rb) * 0.04 / max(a.seconds, 1):.1f})")


main()
