#!/usr/bin/env python3
"""Reconstruct what each earring's microphone hears, as WAV files.

The firmware keeps a byte-wide capture ring in animationBuf (micDumpAdd in
sync.go): 2048 samples at 8 kSPS - 2-tap decimated from the 16 kSPS stream,
centred on a tracked DC level - with the absolute write count in the word
immediately above it. One ascending `mdw` over the region therefore reads the
ring and samples the counter *last*, which is what makes a window read
race-free: every sample still in the ring below that counter is intact.

The counter is the whole point. Reading adcData directly cannot work - that
ring is 15.6 ms deep while a read takes ~100 ms, so the DMA rewrote it mid-read
and the "reconstruction" was noise. Here the buffer holds 256 ms against a
~112 ms read, and a window whose counter says the writer lapped it is reported
as a gap instead of being stitched in with a guessed phase.

Rate budget: OpenOCD sustains ~17 kB/s of mdw output (measured; independent of
adapter clock), so 8-bit at 8 kSPS uses about half the wire and both earrings
can stream at once. Each device's (wall, counter) pairs give a linear clock fit,
so the two files land on one wall-clock timeline despite separate crystals and
different boot times. Playback volume is never touched.

  tools/micdump.py --file /tmp/psync/h900hi.wav [--seconds 97] [--outdir DIR]

Writes OUT/hears-left.wav and OUT/hears-right.wav (8 kHz mono, music at t=0.5 s)
and prints coverage, levels and how well the two agree.
"""
import argparse
import os
import re
import subprocess
import sys
import threading
import time
import wave

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import paircapture
import telem

HOST = "pi"
PORTS = {"L": ("1-1.1", 4445), "R": ("1-1.2", 4446)}
RING = 2048            # micDumpSize: samples (= bytes) in the ring
RATE = 8000            # dump rate = 16 kSPS / micDumpDecim
WORDS = RING // 4 + 3  # ring + DC (Q4) + applied shift + write counter
DC_FRAC = 4            # micDumpDCFrac: fraction bits on the DC estimate
HEXWORD = re.compile(r"(?<![0-9a-fA-Fx])[0-9a-fA-F]{8}(?![0-9a-fA-F])")


def open_ocd(tport, tries=10):
    for _ in range(tries):
        try:
            return telem.OpenOCD("localhost", tport)
        except (ConnectionRefusedError, TimeoutError, OSError):
            time.sleep(1.5)
    raise RuntimeError(f"telnet {tport}: no answer after {tries} tries")


def poll(ocd, base):
    """One round trip: the whole ring plus its control words.

    Returns (wall, counter, dc_q4, shift, clipped, codes) where `codes` are the
    ring's samples converted back to ADC deviations in 16-bit-oversampled codes,
    using the gain shift in force when the poll was taken. The counter sits at
    the top of the region, so the ascending read samples it last and it bounds
    everything below it.

    The firmware stores (dev >> (4 + sh)) where dev is the Q4 deviation, so the
    byte is (counts >> sh) and the reconstruction is << sh - not << (4 + sh).
    The extra 16 was here for a while and turned every loud passage into an
    int16 wrap: the aligned WAVs came back with +-32768 impulses in 10-38 % of
    the samples, which looked like a violent electrical noise floor and is
    purely this multiply.
    """
    text = ocd.cmd(f"mdw 0x{base:08x} {WORDS}")
    t = time.time()
    words = HEXWORD.findall(text)
    if len(words) != WORDS:
        return None
    w = np.array([int(x, 16) for x in words[:RING // 4]], dtype=np.uint32)
    data = np.empty(RING, dtype=np.uint8)
    data[0::4] = w & 0xFF
    data[1::4] = (w >> 8) & 0xFF
    data[2::4] = (w >> 16) & 0xFF
    data[3::4] = (w >> 24) & 0xFF
    raw = data.view(np.int8)
    shift = int(words[RING // 4 + 1], 16) & 0xFF
    counter = int(words[RING // 4 + 2], 16)
    dc = int(words[RING // 4], 16)
    dc -= 1 << 32 if dc >= 1 << 31 else 0
    clipped = int(np.count_nonzero((raw == 127) | (raw == -128)))
    return (t, counter, dc, shift, clipped,
            raw.astype(np.float64) * float(1 << shift))


class Capture:
    """Counter-stamped windows, stitched into one stream of device samples."""

    def __init__(self):
        self.chunks = []   # (absolute sample index, ADC-code array)
        self.last = None   # next unread absolute index
        self.pairs = []    # (wall time, counter) for the clock fit
        self.shifts = []   # (wall time, dump gain shift)
        self.clipped = 0   # samples that hit the byte's endstop
        self.lost = 0      # samples the writer lapped before we read them
        self.bad = 0
        self.lo = 0        # absolute index of stream()[0]

    def add(self, t, counter, dc, shift, clipped, codes):
        self.pairs.append((t, counter))
        self.shifts.append((t, shift))
        self.clipped += clipped
        if self.last is None or counter - self.last > RING:
            # Nothing usable before the writer wrapped: take the newest window.
            if self.last is not None:
                self.lost += counter - self.last - RING
            self.chunks.append((counter - RING, codes.copy()))
            self.last = counter
            return
        n = counter - self.last
        if n <= 0:
            return
        start = self.last % RING
        chunk = np.concatenate((codes[start:], codes[:start]))[:n]
        self.chunks.append((self.last, chunk.copy()))
        self.last = counter

    def stream(self):
        if not self.chunks:
            return np.zeros(0, dtype=np.int16)
        self.lo = min(c[0] for c in self.chunks)
        hi = max(c[0] + len(c[1]) for c in self.chunks)
        # Samples are ADC deviations in 16-bit-oversampled codes, which is
        # already the range an int16 WAV describes: no rescale, so a number read
        # out of the file is a number the pipeline saw.
        out = np.zeros(hi - self.lo)
        for start, ch in self.chunks:
            out[start - self.lo:start - self.lo + len(ch)] = ch
        return np.clip(out, -32768, 32767).astype(np.int16)

    def fit(self):
        """counter = b * wall + c, i.e. the device's sample clock vs wall time."""
        a = np.array(self.pairs)
        b, c = np.polyfit(a[:, 0], a[:, 1], 1)
        return b, c


def capture(name, tport, base, seconds, out_wav):
    ocd = open_ocd(tport)
    cap = Capture()
    t_end = time.time() + seconds
    while time.time() < t_end:
        try:
            r = poll(ocd, base)
        except (TimeoutError, ConnectionError, OSError) as e:
            print(f"{name}: poll failed: {e}", file=sys.stderr)
            time.sleep(0.5)
            continue
        if r is None:
            cap.bad += 1
            continue
        cap.add(*r)
    ocd.close()
    stream = cap.stream()
    span = len(stream)
    filled = sum(len(c[1]) for c in cap.chunks)
    with wave.open(out_wav, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(RATE)
        wf.writeframes(stream.tobytes())
    sh = [s for _, s in cap.shifts]
    print(f"{out_wav}: {span / RATE:.1f} s  coverage="
          f"{100 * min(filled, span) / max(span, 1):.0f}%  lapped={cap.lost} "
          f"bad_polls={cap.bad}  dump gain 2^{min(sh) if sh else '-'}.."
          f"{max(sh) if sh else '-'}  endstopped={cap.clipped} "
          f"({100 * cap.clipped / max(filled, 1):.1f}%)")
    return stream, cap


def on_wall(stream, cap, grid, t0):
    """Resample onto a common wall-clock grid (nearest sample, gaps stay zero)."""
    b, c = cap.fit()
    idx = np.round(b * (grid + t0) + c).astype(np.int64) - cap.lo
    out = np.zeros(len(grid), dtype=np.int16)
    ok = (idx >= 0) & (idx < len(stream))
    out[ok] = stream[idx[ok]]
    return out


def report(l, r):
    n = min(len(l), len(r))
    l, r = l[:n].astype(np.float64), r[:n].astype(np.float64)
    print(f"\nlevels: L rms={np.sqrt((l ** 2).mean()):.0f}  "
          f"R rms={np.sqrt((r ** 2).mean()):.0f}  "
          f"R-L={20 * np.log10(max(np.sqrt((r ** 2).mean()), 1) / max(np.sqrt((l ** 2).mean()), 1)):+.1f} dB")

    def env(x):
        k = RATE // 20
        return np.convolve(np.abs(x), np.ones(k) / k, "same")

    el, er = env(l), env(r)
    best = (0, -9.0)
    for lag in range(-RATE * 2, RATE * 2, RATE // 200):
        i0, i1 = RATE, min(n, RATE * 21)
        j0, j1 = i0 + lag, i1 + lag
        if j0 < 0 or j1 > n:
            continue
        m = (l[i0:i1] != 0) & (r[j0:j1] != 0)
        if m.sum() < RATE:
            continue
        c = np.corrcoef(el[i0:i1][m], er[j0:j1][m])[0, 1]
        if c > best[1]:
            best = (lag, c)
    print(f"envelope agreement: corr {best[1]:+.2f} at lag {best[0] / RATE * 1000:+.0f} ms "
          f"(lag 0: {np.corrcoef(el[:n], er[:n])[0, 1]:+.2f})")
    print("\nband levels during the music (dBFS, and L-R):")
    def band(x):
        v = x[RATE:11 * RATE]
        v = v[v != 0]
        if len(v) < RATE:
            return np.full(8, np.nan)
        f = np.abs(np.fft.rfft((v - v.mean()) * np.hanning(len(v)))) ** 2
        fr = np.fft.rfftfreq(len(v), 1 / RATE)
        edges = [0, 125, 250, 500, 1000, 2000, 4000]
        out = []
        for lo, hi in zip(edges, edges[1:]):
            m = (fr >= lo) & (fr < hi)
            out.append(10 * np.log10(f[m].mean() + 1e-12))
        return np.array(out)
    bl, br = band(l), band(r)
    for (lo, hi), a, b in zip(list(zip(edges, edges[1:])), bl, br):
        print(f"  {lo:4d}-{hi:4d} Hz: L {a:7.1f}  R {b:7.1f}  "
              f"{'L' if a > b else 'R'} {abs(a - b):5.1f} dB")


# module-level band edges, also used by report()
edges = [0, 125, 250, 500, 1000, 2000, 4000]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--file", required=True)
    ap.add_argument("--seconds", type=float, default=97)
    ap.add_argument("--lead", type=float, default=4.0)
    ap.add_argument("--outdir", default="/home/max/Documents/led-earrings")
    args = ap.parse_args()
    os.makedirs(args.outdir, exist_ok=True)

    here = os.path.dirname(os.path.abspath(__file__))
    syms = telem.resolve_symbols(os.path.join(here, "..", "firmware.elf"))
    base = syms[next(k for k in syms if k.endswith("main.animationBuf"))]
    print(f"animationBuf = 0x{base:08x}  ring {RING} samples @ {RATE} Hz")

    fwd = " ".join(f"-L {p}:localhost:{p}" for _, p in PORTS.values())
    subprocess.run(["pkill", "-f", "ssh -o ExitOnForwardFailure.*-L "])
    time.sleep(0.5)
    tunnel = subprocess.Popen(
        f"ssh -o ExitOnForwardFailure=yes -N -n {fwd} {HOST}", shell=True)
    out = {}
    try:
        paircapture.spawn_servers(HOST)
        ths = {}
        for name, (_, tp) in PORTS.items():
            t = threading.Thread(
                target=lambda n=name, p=tp: out.__setitem__(
                    n, capture(n, p, base, args.seconds,
                               os.path.join(args.outdir,
                                           f"hears-raw-{'left' if n == 'L' else 'right'}.wav"))),
                daemon=True)
            t.start()
            ths[name] = t
        play_at = time.time() + args.lead
        time.sleep(max(play_at - time.time(), 0))
        player = subprocess.Popen(["ffplay", "-v", "error", "-nodisp",
                                   "-autoexit", args.file],
                                  stdout=subprocess.DEVNULL,
                                  stderr=subprocess.DEVNULL)
        try:
            for t in ths.values():
                t.join()
        finally:
            player.kill()
    finally:
        tunnel.kill()

    # Put both on one wall-clock grid: t=0 is 0.5 s before the music started.
    t0 = play_at - 0.5 - args.lead
    n = int((args.seconds - args.lead - 0.5) * RATE)
    grid = np.arange(n) / RATE
    aligned = {k: on_wall(s, c, grid, t0) for k, (s, c) in out.items()}
    m = min(len(v) for v in aligned.values())
    for name in ("L", "R"):
        p = os.path.join(args.outdir,
                         f"hears-{'left' if name == 'L' else 'right'}.wav")
        with wave.open(p, "wb") as wf:
            wf.setnchannels(1)
            wf.setsampwidth(2)
            wf.setframerate(RATE)
            wf.writeframes(aligned[name][:m].tobytes())
        print(f"wrote {p} ({m / RATE:.1f} s, music at 0.5 s)")
    report(aligned["L"], aligned["R"])


if __name__ == "__main__":
    main()
