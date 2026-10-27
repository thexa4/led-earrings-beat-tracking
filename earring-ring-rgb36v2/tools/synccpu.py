#!/usr/bin/env python3
"""CPU cost of an uploaded program in sync mode, on both bench earrings.

  tinygo build -tags syncboot -o sb.elf -target=./stm32l0x1.json -opt=2 -panic=trap .
  tinygo build -tags syncboot -o sb.bin -target=./stm32l0x1.json -opt=2 -panic=trap .
  go run ./tools/patternc -o prog.bin prog.c
  tools/synccpu.py sb.elf prog.bin [prog.bin ...] [--builtin] [--seconds 15]

sb.bin must sit next to sb.elf. For each program, the whole flash is written
(firmware, the program in custom slot 0, and a saved state that boots into
sync mode variant 3 = slot 0) through the running OpenOCD servers, then after
--settle seconds both earrings are measured for --seconds:

  passes/s   main-loop passes (main.syncPolls). Each draws 3 LEDs (3 getPixel
             calls) and feeds at most syncFeedCap = 24 samples, so below
             16000/24 = 667 passes/s the analysis falls behind
  frames/s   analysis frames from the sync data block: below the earring's own
             sample rate / 250 (63.7-64) means samples were lost
  ADC/used   samples written (main.syncWraps*250; undercounts a stall longer
             than one ring) and consumed by the pipeline, per second
  backlog    DMA ahead of the reader, host-sampled, in samples (ring = 250)
  profile    DWT PC samples (the M0+ PCSR, read without halting): the share in
             the program (it runs from RAM, in animationBuf) and the top
             firmware functions (syncPoll is the analysis)

--builtin measures the built-in pulse variant first, as a baseline. The last
program measured is left running.
"""
import argparse
import os
import re
import struct
import subprocess
import sys
import tempfile
import threading
import time
from collections import Counter

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem
from hash32 import hash32

PORTS = {"L": 4445, "R": 4446}
SLOT0 = 0x08007980
CODE_END = 0x08006D80  # the firmware ends below the three pattern slots
STATE = 0x08007F80
MODE_SYNC, MODE_CUSTOM0 = 11, 8
PCSR, NDTR = 0xE000101C, 0x4002000C
SHARE = 0x20001FC0


def funcs(elf):
    out = []
    for line in subprocess.run(["readelf", "-sW", elf], capture_output=True, text=True).stdout.splitlines():
        f = line.split()
        if len(f) >= 8 and f[3] in ("FUNC", "OBJECT"):
            out.append((int(f[1], 16), int(f[2]), f[3], f[7]))
    return out


def image(prog, sync_variant):
    slot = struct.pack("<I", len(prog) | (hash32(prog) & 0xFFFF) << 16) + prog
    slot += bytes(1536 - len(slot))
    st = bytearray(32)
    st[0] = MODE_SYNC
    st[2 + MODE_SYNC] = sync_variant
    st[2 + MODE_CUSTOM0] = 0
    st[28:32] = struct.pack("<I", hash32(bytes(st[:28])))
    return slot + bytes(st) + bytes(128 - 32)


def ihex(data, base):
    out = []
    for off in range(0, len(data), 16):
        addr = base + off
        if off == 0 or addr & 0xFFFF == 0:
            rec = bytes([2, 0, 0, 4, addr >> 24 & 0xFF, addr >> 16 & 0xFF])
            out.append(":" + rec.hex().upper() + "%02X" % (-sum(rec) & 0xFF))
        chunk = data[off:off + 16]
        rec = bytes([len(chunk), addr >> 8 & 0xFF, addr & 0xFF, 0]) + chunk
        out.append(":" + rec.hex().upper() + "%02X" % (-sum(rec) & 0xFF))
    return "\n".join(out + [":00000001FF"]) + "\n"


def program(fw_bin, tail, work):
    # OpenOCD erases this flash in 4 kB sectors, and slot 0 + the state page
    # share the last one with the firmware's tail: writing them alone erases
    # the end of the firmware. So every run writes the whole 32 kB image.
    fw = open(fw_bin, "rb").read()
    assert len(fw) <= CODE_END - 0x08000000
    full = fw + bytes(SLOT0 - 0x08000000 - len(fw)) + tail
    assert len(full) == 32768
    open(f"{work}/full.hex", "w").write(ihex(full, 0x08000000))
    remote = "/tmp/cpuload-full.hex"
    subprocess.run(["scp", "-q", f"{work}/full.hex", f"pi:{remote}"], check=True)
    for s, port in PORTS.items():
        o = telem.OpenOCD("localhost", port)
        r = o.cmd(f"program {remote} verify reset")
        o.close()
        if "Verified OK" not in r:
            raise RuntimeError(f"{s}: program failed: {r[-300:]!r}")


def measure(side, syms, fn, seconds, res):
    o = telem.OpenOCD("localhost", PORTS[side])
    A = {k: syms[k] for k in ("main.syncPolls", "main.syncWraps", "main.syncAnimFrame", "main.syncFed")}

    def counters():
        t0 = time.time()
        v = {k: o.mdw(a, 1)[0] for k, a in A.items()}
        sh = o.mdw(SHARE, 3)
        v["frame"] = sh[2]
        v["t"] = (t0 + time.time()) / 2
        v["magic"] = sh[0]
        return v

    c0 = counters()
    pcs, backlog = [], []
    end = time.time() + seconds
    i = 0
    while time.time() < end:
        pcs.append(o.mdw(PCSR, 1)[0])
        i += 1
        if i % 4 == 0:
            # One command line, so both reads run back to back on the
            # flashing host (~1 ms, ~16 samples) instead of a network round
            # trip (~8 ms) apart. syncRead first: the reader only moves
            # forward, so the backlog reads up to ~16 high, never wrapped.
            t = o.cmd(f"echo [mdw 0x{syms['main.syncRead']:08x}][mdw 0x{NDTR:08x}]")
            w = re.findall(r":\s*([0-9a-f]{8})", t)
            if len(w) == 2:
                backlog.append((250 - int(w[1], 16) - int(w[0], 16)) % 250)
    c1 = counters()
    o.close()
    dt = c1["t"] - c0["t"]
    written = (c1["main.syncWraps"] - c0["main.syncWraps"]) * 250
    used = ((c1["main.syncAnimFrame"] - c0["main.syncAnimFrame"]) * 250
            + (c1["main.syncFed"] - c0["main.syncFed"]))
    buf = syms["main.animationBuf"]
    cnt = Counter()
    for pc in pcs:
        pc &= ~1
        if buf[0] <= pc < buf[0] + buf[1]:
            cnt["<program>"] += 1
            continue
        name = "?"
        for a, sz, _, nm in fn:
            if a <= pc < a + max(sz, 2):
                name = nm
                break
        cnt[name] += 1
    res[side] = dict(passes=(c1["main.syncPolls"] - c0["main.syncPolls"]) / dt,
                     frames=(c1["frame"] - c0["frame"]) / dt, written=written / dt, used=used / dt,
                     bl_max=max(backlog), bl_p99=np.percentile(backlog, 99), n=len(pcs), prof=cnt,
                     magic=c1["magic"] == 0x434E5953)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("elf")
    ap.add_argument("progs", nargs="*")
    ap.add_argument("--builtin", action="store_true")
    ap.add_argument("--seconds", type=float, default=20)
    ap.add_argument("--settle", type=float, default=5)
    a = ap.parse_args()
    syms = {}
    fn = []
    for addr, sz, typ, nm in funcs(a.elf):
        syms[nm] = (addr, sz) if nm == "main.animationBuf" else addr
        if typ == "FUNC":
            fn.append((addr & ~1, sz, typ, nm))
    runs = [("builtin pulse", b"\0" * 4, 1)] if a.builtin else []
    runs += [(os.path.basename(p), open(p, "rb").read(), 3) for p in a.progs]
    work = tempfile.mkdtemp(prefix="synccpu-")
    for name, prog, var in runs:
        program(os.path.splitext(a.elf)[0] + ".bin", image(prog, var), work)
        time.sleep(a.settle)
        res = {}
        th = [threading.Thread(target=measure, args=(s, syms, fn, a.seconds, res)) for s in PORTS]
        for t in th:
            t.start()
        for t in th:
            t.join()
        for s in PORTS:
            r = res[s]
            prog_share = r["prof"]["<program>"] / r["n"]
            top = ", ".join(f"{k.replace('main.', '').replace('github.com/aykevl/things/earring-ring-rgb36v2/', '')} {v / r['n']:.0%}"
                            for k, v in r["prof"].most_common(5))
            print(f"{name:14s} {s}: passes {r['passes']:6.0f}/s  frames {r['frames']:5.2f}/s  "
                  f"ADC {r['written']:6.0f}/s used {r['used']:6.0f}/s  backlog max {r['bl_max']:3d} p99 {r['bl_p99']:3.0f}  "
                  f"program {prog_share:4.0%}  [{top}]{'' if r['magic'] else '  NO SYNC BLOCK'}", flush=True)


if __name__ == "__main__":
    main()
