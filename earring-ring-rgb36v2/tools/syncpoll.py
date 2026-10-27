#!/usr/bin/env python3
"""Poll the sync data block (0x20001fc0, syncshare.go) on both earrings with a
shared host clock while one ffplay plays audio here.

  tools/syncpoll.py OUTDIR --seconds 300 [--play podcast.mp3 --ss 1200]

The block sits at a fixed address, so no ELF is needed: any firmware with
sync data works (build with -tags syncboot so the earrings boot into sync).
The OpenOCD servers on the flashing host must be up (4445 = left,
4446 = right, forwarded to localhost; tools/flash.sh kills them, so restart
them after every flash). One read takes ~8 ms, so each earring is polled at
~115/s, nearly twice per analysis frame.

Writes OUTDIR/L.csv, OUTDIR/R.csv - t0,t1 (host time around the read) and
the decoded block fields - and OUTDIR/play.txt (when the player started).
Score with tools/syncpair.py and tools/synckick.py.
"""
import argparse
import os
import struct
import subprocess
import sys
import threading
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem

ADDR = 0x20001FC0
PORTS = {"L": 4445, "R": 4446}
COLS = ("t0 t1 magic version flags pattern phase frame beats sections beatms level bass mid high "
        "secage beatage b0 b1 b2 b3 b4 b5 b6 b7 hash").split()


def decode(w):
    raw = struct.pack("<11I", *w)
    magic, ver, flags, pat, ph, frame, beats, sec, bms, lvl, bass, mid, high, sage, bage = \
        struct.unpack_from("<IBBBBIIIHBBBBBB", raw)
    bands = struct.unpack_from("<8B", raw, 28)
    h = struct.unpack_from("<I", raw, 36)[0]
    return [magic, ver, flags, pat, ph, frame, beats, sec, bms, lvl, bass, mid, high, sage, bage,
            *bands, h]


def poll(side, stop, out, errs):
    ocd = telem.OpenOCD("localhost", PORTS[side])
    with open(out, "w") as f:
        f.write(",".join(COLS) + "\n")
        while not stop.is_set():
            t0 = time.time()
            try:
                w = ocd.mdw(ADDR, 11)
            except Exception:  # keep going; count it
                errs[side] += 1
                time.sleep(0.05)
                try:
                    ocd = telem.OpenOCD("localhost", PORTS[side])
                except Exception:
                    pass
                continue
            t1 = time.time()
            if len(w) != 11:
                errs[side] += 1
                continue
            f.write(",".join(str(v) for v in [f"{t0:.4f}", f"{t1:.4f}", *decode(w)]) + "\n")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("outdir")
    ap.add_argument("--seconds", type=float, default=60)
    ap.add_argument("--play")
    ap.add_argument("--ss", type=float, default=0)
    a = ap.parse_args()
    os.makedirs(a.outdir, exist_ok=True)
    stop = threading.Event()
    errs = {s: 0 for s in PORTS}
    th = [threading.Thread(target=poll, args=(s, stop, f"{a.outdir}/{s}.csv", errs)) for s in PORTS]
    for t in th:
        t.start()
    player = None
    if a.play:
        player = subprocess.Popen(["ffplay", "-v", "error", "-nodisp", "-autoexit",
                                   "-ss", str(a.ss), "-t", str(a.seconds), a.play],
                                  stdin=subprocess.DEVNULL)
        with open(f"{a.outdir}/play.txt", "w") as f:
            f.write(f"{time.time():.4f} {a.play} ss={a.ss}\n")
    try:
        time.sleep(a.seconds)
    finally:
        stop.set()
        for t in th:
            t.join()
        if player:
            player.terminate()
    print("read errors:", errs)


if __name__ == "__main__":
    main()
