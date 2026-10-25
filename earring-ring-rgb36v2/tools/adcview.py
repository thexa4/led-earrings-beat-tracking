#!/usr/bin/env python3
"""Does the ADC itself see the room? Reads the DMA ring straight out of RAM.

The byte dump (micdump.py) is a lossy path: 8-bit samples, a tracked DC level, a
gain that changes between polls, and a 2:1 decimation. Every one of those is a
place a real tone can be destroyed, and one of them already destroyed a whole
afternoon of measurement. This reads the thing the ADC actually wrote - the
uint16 window DMA is filling at 16 kSPS in sync.go's startSync - so there is
nothing left to blame but the converter and its pin.

Each poll is one ascending mdw of the window: 250 samples = 15.6 ms at 16 kSPS.
The window is circular, so a poll is a snapshot of whatever 15.6 ms happened to
be in flight, which is fine: the question is only whether the coherent amplitude
at the tone frequency is bigger while the tone is playing.

On the flashing host with both earrings on ST-Links:

  python3 tools/adcview.py --elf firmware.elf --ports 4445 4446 \
      --names L R --seconds 40 --tone 1000 --file /tmp/psync/tones96.wav
"""

import argparse
import os
import statistics
import sys
import threading
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import paircapture  # noqa: E402
import telem  # noqa: E402

HOST = os.environ.get("FLASH_HOST", "pi")
WINDOW = 250  # syncWindowSize: samples in the circular DMA window


def player(path):
    import subprocess
    p = subprocess.Popen(["ffplay", "-nodisp", "-autoexit", "-loglevel", "error", path],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return p


def coherent(x, fs, f):
    """Amplitude of a sinusoid at f, in the same units as x, best over +-3 %."""
    n = len(x)
    w = np.hanning(n)
    sw = w.sum()
    t = np.arange(n) / fs
    best = 0.0
    for ff in np.linspace(f * 0.97, f * 1.03, 13):
        if ff > 0.95 * fs / 2:
            continue
        v = 2 * abs(((x - x.mean()) * w) * np.exp(-2j * np.pi * ff * t)).sum() / sw
        best = max(best, float(v))
    return best


def poll_window(ocd, base):
    """The circular DMA window as the ADC wrote it: 250 x uint16."""
    words = ocd.mdw(base, WINDOW // 2)
    if len(words) < WINDOW // 2:
        return None
    w = np.array(words, dtype=np.uint32)
    x = np.empty(WINDOW, dtype=np.uint16)
    x[0::2] = (w & 0xFFFF).astype(np.uint16)
    x[1::2] = (w >> 16).astype(np.uint16)
    return x.astype(np.float64)


def worker(port, base, out, stop):
    ocd = telem.OpenOCD(HOST, port)
    while time.time() < stop:
        try:
            x = poll_window(ocd, base)
        except Exception as e:  # noqa: BLE001
            print(f"port {port}: {e}", file=sys.stderr)
            time.sleep(0.2)
            continue
        if x is not None:
            out.append((time.time(), x))
    ocd.close()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--elf", default="firmware.elf")
    ap.add_argument("--ports", type=int, nargs="+", default=[4445, 4446])
    ap.add_argument("--names", nargs="+", default=None)
    ap.add_argument("--seconds", type=float, default=64)
    ap.add_argument("--tone", type=float, default=1000,
                    help="frequency to project onto")
    ap.add_argument("--split", type=float, default=30.0,
                    help="seconds of tone at the start of --file; polls before the "
                         "playback start plus this are 'on', after are 'off'")
    ap.add_argument("--file", default="", help="play this while sampling")
    a = ap.parse_args()
    names = a.names or [f"p{p}" for p in a.ports]

    base = telem.resolve_symbols(a.elf)["main.adcData"]
    print(f"adcData = 0x{base:08x}  window {WINDOW} samples @ 16000 Hz")

    # Playback starts first, and its exact start is found from the data rather
    # than assumed: the amplitude trace below shows the step, and the on/off
    # means are reported for a window around the blocks so a fraction of a
    # second of slop at either edge cannot invent a tone.
    proc = player(a.file) if a.file else None
    play_t0 = time.time()
    if proc:
        time.sleep(1.0)
    stop = time.time() + a.seconds
    data = {}
    th = []
    for name, port in zip(names, a.ports):
        data[name] = []
        t = threading.Thread(target=worker, args=(port, base, data[name], stop))
        t.start()
        th.append(t)
    for t in th:
        t.join()
    if proc:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except Exception:  # noqa: BLE001
            proc.kill()

    print(f"\ncaptured " + ", ".join(f"{n}={len(data[n])}" for n in names)
          + f" windows of {WINDOW / 16:.1f} ms over {a.seconds:.0f} s")
    print(f"tone block: {a.split:.0f} s from playback start, then silence\n")
    for name in names:
        rows = [(ts - play_t0, coherent(x, 16000.0, a.tone),
                 float(np.std(x)), float(np.mean(x))) for ts, x in data[name]]
        print(f"{name}: coherent amplitude at {a.tone:.0f} Hz, raw 16-bit ADC codes")
        print(f"{'  t':>7} {'amp':>8} {'noise':>8} {'code':>8}"
              f"   {'t':>7} {'amp':>8} {'noise':>8} {'code':>8}")
        half = (len(rows) + 1) // 2
        for i in range(half):
            left = rows[i]
            right = rows[i + half] if i + half < len(rows) else None
            s = f"{left[0]:7.2f} {left[1]:8.1f} {left[2]:8.1f} {left[3]:8.0f}"
            if right:
                s += f"   {right[0]:7.2f} {right[1]:8.1f} {right[2]:8.1f} {right[3]:8.0f}"
            print(s)
        on = [r[1] for r in rows if a.split * 0.2 < r[0] < a.split * 0.9]
        off = [r[1] for r in rows if a.split * 1.15 < r[0] < a.split * 1.85]
        if on and off:
            print(f"  tone: mean={statistics.mean(on):.1f} "
                  f"min={min(on):.1f} max={max(on):.1f} (n={len(on)})   "
                  f"silence: mean={statistics.mean(off):.1f} "
                  f"min={min(off):.1f} max={max(off):.1f} (n={len(off)})")
            print(f"  rise = {20 * np.log10(max(statistics.mean(on), 1e-9) / max(statistics.mean(off), 1e-9)):+.1f} dB")
        print(f"  idle code ≈ {statistics.mean([r[3] for r in rows]):.0f}, "
              f"sample-to-sample noise ≈ "
              f"{statistics.mean([r[2] for r in rows]):.1f} codes")


main()
