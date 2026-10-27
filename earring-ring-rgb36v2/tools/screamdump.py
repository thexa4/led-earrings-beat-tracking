#!/usr/bin/env python3
"""Record what the earring microphones hear in the data receiver's capture
configuration, from firmware built with -tags screamdump (screamdump.go).

  tools/screamdump.py --elf screamdump.elf --seconds 60 --play scream.wav \\
      --cfg 0 --out captures/scream/NAME

The firmware keeps a 2400-byte mu-law ring (240 ms at 10 kSPS) in
animationBuf with a sample counter above it. Each poll reads the counter
(C0), then only the bytes written since the last poll, then the counter
again (C1): a byte at index i is intact iff i >= C1 - RING. So unlike a
whole-ring read nothing is ever spliced in stale, and a lapped stretch is
recorded as lost (zero-filled, and counted), not guessed.

read_memory sustains ~19 kB/s per ST-Link and the two run in parallel, so
both earrings record at once (10 kB/s each). Audio is played HERE by one
ffplay for both devices; the volume is never touched.

Writes OUT/L.wav, OUT/R.wav (10 kHz mono int16, in ADC codes relative to the
tracked DC, i.e. what the receiver's DC removal sees) and OUT/meta.json:
config, shift, counter/wall-clock fit (the true sample rate), lost and
clipped counts, and the sample index at which the player was started.
"""
import argparse
import json
import os
import re
import subprocess
import sys
import threading
import time
import wave

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem

PORTS = {"L": 4445, "R": 4446}
RING = 2400
RING_WORDS = RING // 4
# Control words after the ring (screamdump.go).
CFG_REQ, CFG, SHIFT, DC, CLIPPED, TOGGLE, COUNT = range(7)
HEX = re.compile(r"0x[0-9a-fA-F]+")


def mulaw_table():
    t = np.zeros(256, dtype=np.int32)
    for c in range(256):
        u = ~c & 0xFF
        sign, exp, mant = u & 0x80, (u >> 4) & 7, u & 0xF
        v = (((mant << 1) + 33) << exp) - 33
        t[c] = -v if sign else v
    return t


MULAW = mulaw_table()


class Device:
    def __init__(self, side, base, args):
        self.side = side
        self.base = base
        self.ctl = base + 4 * RING_WORDS
        self.ocd = telem.OpenOCD("localhost", PORTS[side])
        self.args = args
        self.chunks = []  # (absolute index, bytes)
        self.pairs = []   # (wall, counter)
        self.lost = 0
        self.polls = 0
        self.last = None
        self.dcs = []

    def words(self, addr, n):
        out = []
        while n > 0:
            k = min(n, 1024)
            text = self.ocd.cmd(f"read_memory 0x{addr:08x} 32 {k}")
            reply = "\n".join(l for l in text.splitlines() if "read_memory" not in l)
            w = [int(x, 16) for x in HEX.findall(reply)]
            if len(w) != k:
                raise RuntimeError(f"{self.side}: read_memory gave {len(w)}/{k}: {text[:200]!r}")
            out += w
            addr += 4 * k
            n -= k
        return out

    def word(self, i):
        return self.words(self.ctl + 4 * i, 1)[0]

    def put(self, i, v):
        self.ocd.cmd(f"mww 0x{self.ctl + 4 * i:08x} {v}")

    def ring_bytes(self, lo, hi):
        """Ring contents for absolute sample indices [lo, hi), hi-lo <= RING."""
        out = bytearray()
        while lo < hi:
            p = lo % RING
            n = min(hi - lo, RING - p)
            w0, w1 = p // 4, (p + n + 3) // 4
            ws = self.words(self.base + 4 * w0, w1 - w0)
            b = np.array(ws, dtype="<u4").tobytes()
            out += b[p - 4 * w0:p - 4 * w0 + n]
            lo += n
        return bytes(out)

    def configure(self):
        a = self.args
        self.put(SHIFT, a.shift)
        self.put(TOGGLE, 1 if a.toggle else 0)
        self.put(CFG_REQ, a.cfg)
        for _ in range(50):
            if self.word(CFG) == a.cfg:
                break
            time.sleep(0.05)
        else:
            raise RuntimeError(f"{self.side}: config {a.cfg} not taken")
        time.sleep(0.3)  # let the DC tracker settle on the new config
        self.clipped0 = self.word(CLIPPED)

    def poll(self):
        c0 = self.word(COUNT)
        t = time.time()
        self.pairs.append((t, c0))
        if self.last is None:
            self.last = c0
            return
        lo = self.last
        if c0 - lo > RING:
            self.lost += c0 - RING - lo
            lo = c0 - RING
        data = self.ring_bytes(lo, c0)
        c1 = self.word(COUNT)
        valid_from = c1 - RING
        if valid_from > lo:
            cut = valid_from - lo
            self.lost += cut
            data = data[cut:]
            lo = valid_from
        if data:
            self.chunks.append((lo, data))
        self.last = c0
        self.polls += 1
        if self.polls % 20 == 0:
            self.dcs.append(self.word(DC))

    def stream(self):
        start = self.chunks[0][0]
        end = self.chunks[-1][0] + len(self.chunks[-1][1])
        out = np.zeros(end - start, dtype=np.int32)
        for i, d in self.chunks:
            out[i - start:i - start + len(d)] = MULAW[np.frombuffer(d, dtype=np.uint8)]
        return start, out << self.args.shift


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--elf", required=True)
    ap.add_argument("--sides", default="LR")
    ap.add_argument("--seconds", type=float, default=30)
    ap.add_argument("--play", help="audio file, played once by ffplay")
    ap.add_argument("--lead", type=float, default=1.0, help="seconds recorded before playing")
    ap.add_argument("--cfg", type=int, default=0)
    ap.add_argument("--shift", type=int, default=1)
    ap.add_argument("--toggle", action="store_true", help="toggle A11 per symbol")
    ap.add_argument("--out", required=True)
    ap.add_argument("--note", default="")
    args = ap.parse_args()

    base = telem.resolve_symbols(args.elf)["main.animationBuf"]
    devs = [Device(s, base, args) for s in args.sides]
    for d in devs:
        d.configure()

    stop = threading.Event()
    errors = []

    def run(d):
        try:
            while not stop.is_set():
                d.poll()
                time.sleep(0.02)
        except Exception as e:  # noqa: BLE001 - report, keep the other side
            errors.append(f"{d.side}: {e}")

    threads = [threading.Thread(target=run, args=(d,)) for d in devs]
    for t in threads:
        t.start()
    player = None
    t_play = None
    try:
        time.sleep(args.lead)
        if args.play:
            t_play = time.time()
            player = subprocess.Popen(["ffplay", "-v", "error", "-nodisp", "-autoexit", args.play],
                                      stdin=subprocess.DEVNULL)
        time.sleep(args.seconds)
    finally:
        stop.set()
        for t in threads:
            t.join()
        if player:
            player.terminate()
            try:
                player.wait(timeout=5)
            except subprocess.TimeoutExpired:
                player.kill()

    os.makedirs(args.out, exist_ok=True)
    meta = dict(cfg=args.cfg, shift=args.shift, toggle=args.toggle, play=args.play,
                lead=args.lead, seconds=args.seconds, note=args.note, elf=args.elf,
                errors=errors, devices={})
    for d in devs:
        if not d.chunks:
            print(f"{d.side}: no data", file=sys.stderr)
            continue
        start, x = d.stream()
        t, c = np.array(d.pairs).T
        rate, c0 = np.polyfit(t - t[0], c, 1)
        play_index = None
        if t_play is not None:
            play_index = int(round(c0 + rate * (t_play - t[0]))) - start
        clipped = d.word(CLIPPED) - d.clipped0
        with wave.open(os.path.join(args.out, f"{d.side}.wav"), "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(10000)
            w.writeframes(np.clip(x, -32768, 32767).astype("<i2").tobytes())
        dc = np.array(d.dcs, dtype=np.int64) / 16 if d.dcs else np.array([0.0])
        mad = float(np.median(np.abs(x - np.median(x))))
        meta["devices"][d.side] = dict(
            samples=len(x), rate=rate, lost=d.lost, clipped=clipped, polls=d.polls,
            play_index=play_index, dc_mean=float(dc.mean()), dc_std=float(dc.std()),
            mad=mad, rms=float(np.sqrt(np.mean(x.astype(float) ** 2))),
            peak=int(np.abs(x).max()))
        print(f"{d.side}: {len(x)} samples, rate {rate:.1f}/s, lost {d.lost}, "
              f"clipped {clipped}, MAD {mad:.1f}, rms {meta['devices'][d.side]['rms']:.1f}, "
              f"peak {meta['devices'][d.side]['peak']}, DC {dc.mean():.0f}")
        d.ocd.close()
    with open(os.path.join(args.out, "meta.json"), "w") as f:
        json.dump(meta, f, indent=1)
    if errors:
        print("errors:", errors, file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
