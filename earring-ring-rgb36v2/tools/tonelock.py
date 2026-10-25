#!/usr/bin/env python3
"""Coherent tone response, measured on a window the DMA cannot rewrite.

Why this exists: adcview.py does the same measurement on an unfrozen ring and
therefore measures the read, not the microphone. Each mdw of 250 samples takes
7-15 ms over OpenOCD while the DMA writes 112-240 more samples into the same
buffer, so the array is a splice of two non-adjacent time segments. A Hann-windowed
DFT across that discontinuity has no coherent peak left to find, which is how a
working microphone got reported as producing "+0.1 dB" - see freeze.py, which
documents the same trap for impulse counts, and sync_micdump.go, which exists
because of it.

The fix is freeze.py's, and only its safe half: clear TIM22 CR1.CEN so no
conversion is requested, read, set CEN again. (Clearing the DMA channel instead
bricks the request stream and needs a power cycle - do not "improve" this that
way.) With the trigger held off the 250 samples really are 15.6 ms of one
contiguous signal, so coherent amplitude at the tone frequency is a valid
measurement of what the pin did.

Labels are not taken on trust. AirPlay resume latency moves playback start by a
fraction of a second to a few, so the tool sweeps the phase offset of the on/off
grid and reports the offset that best separates the two classes. A microphone
that transduces the tone has to show a large separation at some offset; one that
does not stays flat for every offset. The unfrozen measurement is reported
alongside for the same poll so the size of the artifact is visible rather than
argued about.

  FLASH_HOST=localhost python3 tools/tonelock.py --file /tmp/psync/tonerpg.wav \
      --seconds 75 --lead 6 --tone 1000 --period 10
"""
import argparse
import math
import os
import sys
import threading
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem  # noqa: E402

TIM22 = 0x40011400
TIM_CR1 = TIM22 + 0x00
CEN = 1
DMA1 = 0x40020000
DMA_CNDTR1 = DMA1 + 0x0C
WINDOW = 250  # syncWindowSize
FS = 16000.0


def coherent(x, f):
    """Amplitude of a sinusoid at f, best over +-3 %, in units of x."""
    n = len(x)
    w = np.hanning(n)
    t = np.arange(n) / FS
    xc = x - x.mean()
    best = 0.0
    for ff in np.linspace(f * 0.97, f * 1.03, 13):
        v = 2 * abs(((xc * w) * np.exp(-2j * np.pi * ff * t)).sum()) / w.sum()
        best = max(best, float(v))
    return best


def window_u16(ocd, base):
    """The ring as uint16 samples, both halves of each word, unsigned."""
    words = ocd.mdw(base, WINDOW // 2)
    if len(words) < WINDOW // 2:
        return None
    w = np.array(words, dtype=np.uint32)
    x = np.empty(WINDOW, dtype=np.uint16)
    x[0::2] = (w & 0xFFFF).astype(np.uint16)
    x[1::2] = (w >> 16).astype(np.uint16)
    return x.astype(np.float64)


def grab(ocd, base):
    """(frozen window, running window, freeze verified)."""
    cr1 = ocd.mdw(TIM_CR1, 1)[0]
    ocd.cmd("mww 0x%08x %d" % (TIM_CR1, cr1 & ~CEN))
    time.sleep(0.008)
    a = ocd.mdw(DMA_CNDTR1, 1)[0]
    fz = window_u16(ocd, base)
    b = ocd.mdw(DMA_CNDTR1, 1)[0]
    ocd.cmd("mww 0x%08x %d" % (TIM_CR1, cr1 | CEN))
    time.sleep(0.008)
    rn = window_u16(ocd, base)
    return fz, rn, (a == b)


def separation(times, amps, period, t0):
    """Median(on) / median(off) in dB for a grid at offset t0. None if empty."""
    on, off = [], []
    for t, a in zip(times, amps):
        if (t - t0) % period < period / 2:
            on.append(a)
        else:
            off.append(a)
    if len(on) < 3 or len(off) < 3:
        return None
    mo, mf = float(np.median(on)), float(np.median(off))
    return math.log2(max(mo, 1e-9) / max(mf, 1e-9)), mo, mf


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default=os.environ.get("FLASH_HOST", "pi"))
    ap.add_argument("--elf", default="firmware.elf")
    ap.add_argument("--file", default="/tmp/psync/tonerpg.wav")
    ap.add_argument("--seconds", type=float, default=75.0)
    ap.add_argument("--lead", type=float, default=6.0,
                    help="seconds of capture before playback starts")
    ap.add_argument("--tone", type=float, default=1000.0)
    ap.add_argument("--period", type=float, default=10.0,
                    help="on+off cycle length in seconds")
    ap.add_argument("--no-play", action="store_true",
                    help="capture only; the room is already playing the fixture")
    a = ap.parse_args()

    syms = telem.resolve_symbols(a.elf)
    base = syms[next(k for k in syms if k.endswith("main.adcData"))]
    print(f"adcData = 0x{base:08x} from {a.elf}")

    targets = [(name, telem.OpenOCD(a.host, port))
               for port, name in ((4445, "L"), (4446, "R"))]
    data = {n: {"t": [], "fz": [], "rn": [], "std": [], "ok": 0} for n, _ in targets}
    stop = time.time() + a.seconds

    player = None
    t_play = None

    def worker(name, o):
        while time.time() < stop:
            try:
                fz, rn, ok = grab(o, base)
            except Exception as e:  # noqa: BLE001
                print(f"{name}: {e}", file=sys.stderr)
                time.sleep(0.3)
                continue
            if fz is None or rn is None:
                time.sleep(0.2)
                continue
            d = data[name]
            d["t"].append(time.time() - t0)
            d["fz"].append(coherent(fz, a.tone))
            d["rn"].append(coherent(rn, a.tone))
            d["std"].append(float(np.std(fz)))
            d["ok"] += 1 if ok else 0
            time.sleep(0.25)

    t0 = time.time()
    threads = [threading.Thread(target=worker, args=(n, o), daemon=True)
               for n, o in targets]
    for th in threads:
        th.start()
    time.sleep(a.lead)
    if not a.no_play:
        import subprocess
        player = subprocess.Popen(
            ["ffplay", "-nodisp", "-autoexit", "-loglevel", "error", a.file],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        t_play = time.time() - t0
        print(f"playback started at t={t_play:.2f}s "
              f"({a.file}, {a.tone:.0f} Hz, {a.period:.0f} s cycle)")
    for th in threads:
        th.join()

    print(f"\n{'dev':4s} {'polls':>5s} {'freeze held':>11s}   "
          f"{'best on/off, FROZEN':>26s}   {'same, UNFROZEN':>20s}")
    for name, o in targets:
        d = data[name]
        n = len(d["t"])
        if n < 8:
            print(f"{name:4s} {n:5d}   too few polls")
            continue
        best_f, best_o = None, None
        for off in np.arange(0, a.period, 0.1):
            s = separation(d["t"], d["fz"], a.period, (t_play or 0) + off)
            if s and (best_f is None or abs(s[0]) > abs(best_f[0])):
                best_f = s
                best_o = off
            s2 = separation(d["t"], d["rn"], a.period, (t_play or 0) + off)
            if s2 and (best_o == off or best_o is None):
                best_r = s2
        if best_f is None:
            print(f"{name:4s} {n:5d} {d['ok']:11d}   not enough on/off samples")
            continue
        bf = f"{best_f[0]:+.2f} dB ({best_f[1]:.0f} vs {best_f[2]:.0f})"
        br = (f"{best_r[0]:+.2f} dB" if best_r else "n/a")
        print(f"{name:4s} {n:5d} {d['ok']:7d}/{n:4d}   {bf:>26s}   {br:>20s}"
              f"  at offset +{best_o:.1f}s")
        print(f"     frozen-window std: median {np.median(d['std']):.0f}"
              f"  p5 {np.percentile(d['std'],5):.0f}"
              f"  p95 {np.percentile(d['std'],95):.0f}")
    for name, o in targets:
        o.cmd("mww 0x%08x %d" % (TIM_CR1, o.mdw(TIM_CR1, 1)[0] | CEN))
        c = [o.mdw(DMA_CNDTR1, 1)[0] for _ in range(2)]
        print(f"{name}: DMA CNDTR {c[0]} -> {c[1]} "
              f"({'advancing, capture restored' if c[0] != c[1] else 'STOPPED'})")
        o.close()
    if player:
        try:
            player.terminate()
        except ProcessLookupError:
            pass


main()
