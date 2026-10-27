#!/usr/bin/env python3
"""Main-loop passes/s in each mode on one bench earring: its LED brightness.

  tinygo build -tags passcount -o pc.elf -target=./stm32l0x1.json -opt=2 -panic=trap .
  tinygo build -tags passcount -o pc.bin -target=./stm32l0x1.json -opt=2 -panic=trap .
  tools/passrate.py pc.elf [--modes 0-11] [--side L] [--seconds 10] [--settle 3]
                    [--restore fw.bin]

pc.bin must sit next to pc.elf. The LEDs are lit only while a pass refreshes
them, for the same time every pass, so passes/s is how bright they are (and,
outside sync mode, how fast the animation runs: a frame is 12 passes). Compare
two builds mode by mode, on the same earring.

For each mode the whole flash is written - the firmware, then the earring's
own tail (both pattern slots, read once at the start) with a state page that
boots into that mode, variant 0 - and after --settle seconds main.benchPasses
is read over --seconds. A mode the firmware doesn't have boots into trace.
--restore writes that firmware and the original tail back at the end;
otherwise the bench build is left running the last mode.
"""
import argparse
import os
import struct
import subprocess
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem
from hash32 import hash32
from synccpu import PORTS, funcs, ihex

TAIL = 0x08007380  # pattern slot 1, slot 0, then the state page
STATE = 0x08007F80
NAMES = ["trace", "noise", "fire", "fire sound", "static", "flag", "sparkle",
         "VU meter", "custom 0", "custom 1", "sync", "basic"]


def program(port, fw, tail, work):
    # The whole 32 kB: slots and state share the last 4 kB erase sector with
    # the firmware's tail (BENCH.md).
    full = fw + b"\xff" * (TAIL - 0x08000000 - len(fw)) + tail
    assert len(full) == 32768, len(full)
    open(f"{work}/full.hex", "w").write(ihex(full, 0x08000000))
    remote = "/tmp/passrate-full.hex"
    subprocess.run(["scp", "-q", f"{work}/full.hex", f"pi:{remote}"], check=True)
    o = telem.OpenOCD("localhost", port)
    r = o.cmd(f"program {remote} verify reset")
    o.close()
    if "Verified OK" not in r:
        raise RuntimeError(f"program failed: {r[-300:]!r}")


def with_mode(tail, mode):
    st = bytearray(tail[STATE - TAIL:STATE - TAIL + 32])
    st[0] = mode
    st[2 + mode] = 0
    st[28:32] = struct.pack("<I", hash32(bytes(st[:28])))
    return tail[:STATE - TAIL] + bytes(st) + tail[STATE - TAIL + 32:]


def modes(spec):
    out = []
    for part in spec.split(","):
        lo, _, hi = part.partition("-")
        out += range(int(lo), int(hi or lo) + 1)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("elf")
    ap.add_argument("--modes", type=modes, default=modes("0-11"))
    ap.add_argument("--side", choices=PORTS, default="L")
    ap.add_argument("--seconds", type=float, default=10)
    ap.add_argument("--settle", type=float, default=3)
    ap.add_argument("--restore", metavar="FW_BIN")
    a = ap.parse_args()
    counter = [addr for addr, _, _, nm in funcs(a.elf) if nm == "main.benchPasses"]
    if not counter:
        sys.exit(f"{a.elf}: no main.benchPasses (build with -tags passcount)")
    fw = open(os.path.splitext(a.elf)[0] + ".bin", "rb").read()
    port = PORTS[a.side]
    o = telem.OpenOCD("localhost", port)
    tail = b"".join(struct.pack("<I", w) for w in o.mdw(TAIL, (0x08008000 - TAIL) // 4))
    o.close()
    work = tempfile.mkdtemp(prefix="passrate-")
    for mode in a.modes:
        program(port, fw, with_mode(tail, mode), work)
        time.sleep(a.settle)
        o = telem.OpenOCD("localhost", port)
        t0, n0 = time.time(), o.mdw(counter[0], 1)[0]
        time.sleep(a.seconds)
        t1, n1 = time.time(), o.mdw(counter[0], 1)[0]
        o.close()
        name = NAMES[mode] if mode < len(NAMES) else "?"
        print(f"{mode:2d} {name:10s} {a.side}: {(n1 - n0) / (t1 - t0):8.1f} passes/s", flush=True)
    if a.restore:
        program(port, open(a.restore, "rb").read(), tail, work)
        print(f"restored {a.restore} and the original tail")


if __name__ == "__main__":
    main()
