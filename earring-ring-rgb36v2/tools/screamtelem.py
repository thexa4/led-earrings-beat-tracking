#!/usr/bin/env python3
"""Watch the receiver telemetry of firmware built with -tags screamtelem
(screamtelem.go): the earrings run the live data receiver in a loop and count
what happens. Optionally plays an audio file (once, by ffplay; the volume is
never touched).

  tools/screamtelem.py --elf screamtelem.elf --seconds 120 --play scream.wav

Prints one line per side every --every seconds and a summary at the end.
"""
import argparse
import os
import struct
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem

PORTS = {"L": 4445, "R": 4446}
MAGIC = 0x5343524D
# screamTelemetry: 6 uint32, int32 max_backlog, uint32, 2 int32, then screamrx.Stats (9 uint16, pad, uint32).
FIELDS = ("magic windows received last_hash last_len last_win max_backlog overruns "
          "trim gain").split()
STATS = "syncs headers ok repaired chased failed lost rounds symfix".split()
WORDS = 10 + 6


def read(ocd, addr):
    w = ocd.mdw(addr, WORDS)
    raw = struct.pack("<%dI" % WORDS, *w)
    v = dict(zip(FIELDS, struct.unpack_from("<6Ii1I2i", raw)))
    v.update(zip(STATS, struct.unpack_from("<9H", raw, 40)))
    v["spikes"] = struct.unpack_from("<I", raw, 60)[0]
    return v


def line(side, v):
    return (f"{side}: win {v['windows']:6d} rx {v['received']} (last {v['last_len']} B "
            f"{v['last_hash']:08x} in {v['last_win'] / 50:.1f}s) | syncs {v['syncs']} hdr {v['headers']} "
            f"ok {v['ok']} rep {v['repaired']} chase {v['chased']} symfix {v['symfix']} "
            f"fail {v['failed']} lost {v['lost']} rounds {v['rounds']} | spikes {v['spikes']} "
            f"gain {v['gain']} trim {v['trim']:+d} backlog {v['max_backlog']} overruns {v['overruns']}")


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--elf", required=True)
    ap.add_argument("--sides", default="LR")
    ap.add_argument("--seconds", type=float, default=60)
    ap.add_argument("--every", type=float, default=5)
    ap.add_argument("--play", help="audio file, played once by ffplay")
    args = ap.parse_args()

    addr = telem.resolve_symbols(args.elf)["main.screamTelemetry"]
    ocds = {s: telem.OpenOCD("localhost", PORTS[s]) for s in args.sides}
    for s, o in ocds.items():
        v = read(o, addr)
        if v["magic"] != MAGIC:
            sys.exit(f"{s}: no screamtelem firmware (magic {v['magic']:08x})")
    player = None
    if args.play:
        player = subprocess.Popen(["ffplay", "-v", "error", "-nodisp", "-autoexit", args.play],
                                  stdin=subprocess.DEVNULL)
    end = time.time() + args.seconds
    try:
        while time.time() < end:
            time.sleep(args.every)
            for s, o in ocds.items():
                print(line(s, read(o, addr)), flush=True)
    finally:
        if player:
            player.terminate()
            player.wait()


if __name__ == "__main__":
    main()
