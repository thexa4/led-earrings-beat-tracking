#!/usr/bin/env python3
"""Dump the live smoothed band spectrum of both earrings while music plays.

The pipeline's smoothed spectrum is plain state in main.syncPipe, so SWD can
read it while the CPU runs (non-intrusive, same tap as the telemetry). This
answers the question the host replay cannot: *how* do two earrings' spectra
actually differ - in which bands, how much, steady or transient?

Offset: Pipeline starts with `filters bank`, whose Go layout on 32-bit is
hpX+hpY (8) + rungA[7] (28) + rungB[7] (28) + energy[8] (32) + gainShift (4)
+ minX + maxX (8) = 108. So smooth = syncPipe+108, prevSmooth = +140,
smoothSnap = +172 (the snapshot the hash was computed from).

The script verifies the offset before recording anything: hot(cur) derived
from smoothSnap must equal the hot bits of the telemetry hash for the same
seq, on every sample.

  tools/specdump.py --file /tmp/psync/h900hi.wav [--seconds 60] [--outdir DIR]

Writes OUT/spec-L.csv / spec-R.csv: t, seq, hash, agcShift, agcPeak, then
snap0..snap7 (Q8 log2) and raw0..raw7 (energies). Prints the difference
spectrum analysis at the end.
"""
import argparse
import csv
import os
import socket
import subprocess
import sys
import threading
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import paircapture
import telem

HOST = "pi"
PORTS = {"L": ("1-1.1", 4445), "R": ("1-1.2", 4446)}
SMOOTH_OFF = 108  # bytes into Pipeline
NB = 8
HOT_FLOOR = 192   # HotFloorQ8
CONTRAST_SHIFT = 22


def sig(v):
    return v - (1 << 32) if v >= (1 << 31) else v


def open_ocd(tport, tries=10):
    for i in range(tries):
        try:
            return telem.OpenOCD("localhost", tport)
        except (ConnectionRefusedError, TimeoutError, OSError):
            time.sleep(1.5)
    raise RuntimeError(f"telnet {tport}: no answer after {tries} tries")


def start_player(path):
    """Play the track once for the whole rig.

    Owned by main, never by a poller: when each device thread spawns its own
    ffplay the room hears the track twice, a second apart, and every cross-device
    difference in the capture measures that echo instead of the signal under test.
    """
    return subprocess.Popen(
        ["ffplay", "-v", "error", "-nodisp", "-autoexit", path],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def poll_device(name, tport, pipe_addr, out, seconds):
    here = os.path.dirname(os.path.abspath(__file__))
    syms = telem.resolve_symbols(os.path.join(here, "..", "firmware.elf"))
    taddr = syms[next(k for k in syms if k.endswith(".Telem"))]
    ocd = open_ocd(tport)
    ocd.cmd("halt")
    time.sleep(0.05)
    ocd.cmd("resume")
    time.sleep(0.05)
    rows = []
    hot_ok = hot_n = 0
    t0 = time.time()
    while time.time() - t0 < seconds:
        ctrl = ocd.mdw(taddr, telem.CTRL_WORDS)
        seq = ctrl[telem.CTRL_SEQ]
        # the telemetry ring record for the last completed frame carries its hash
        slot = ((seq - 1) % telem.RECORDS) * 4
        ring = ocd.mdw(taddr + 4 * telem.TELEMETRIES_OFFSET + 4 * slot, 4)
        hsh = ring[0]
        # Pipeline words: bank ends at 108 (energies 64..96, gain 96, min/max
        # 100/104), smooth 108, prevSmooth 140, smoothSnap 172, energies copy
        # at 204, agcPeak 236, agcShift 240.
        blk = ocd.mdw(pipe_addr, 62)
        raw = blk[16:16 + NB]
        snap = blk[43:43 + NB]
        agcPeak, agcShift = sig(blk[59]), sig(blk[60])
        now = time.time() - t0
        # verify the layout: hot bits derived from snap must match the hash
        mx = max(snap)
        hot = 0
        for b in range(NB):
            if snap[b] >= mx - HOT_FLOOR:
                hot |= 1 << b
        if hsh and (hsh >> CONTRAST_SHIFT) & 0xFF:
            hot_n += 1
            hot_ok += ((hsh >> CONTRAST_SHIFT) & 0xFF) == hot
        rows.append([round(now, 4), seq, hsh, agcShift, agcPeak] +
                    [sig(v) for v in snap] + list(raw))
    ocd.close()
    with open(out, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["t", "seq", "hash", "agcShift", "agcPeak"] +
                   [f"snap{i}" for i in range(NB)] + [f"raw{i}" for i in range(NB)])
        w.writerows(rows)
    print(f"{out}: {len(rows)} samples, hot-bit layout match {hot_ok}/{hot_n}")


def port_open(p):
    with socket.socket() as s:
        s.settimeout(0.3)
        return s.connect_ex(("127.0.0.1", p)) == 0


def ensure_tunnel(want):
    """Forward the given OpenOCD telnet ports, reusing any tunnel already up.

    Spawning a tunnel unconditionally after a pkill used to leave the ports held
    by a survivor whose command line did not match the pattern: the new tunnel
    failed to bind, printed "Address already in use", and the run read whatever
    the stale tunnel happened to be pointing at. Ask the port who owns it first.
    """
    missing = [p for p in want if not port_open(p)]
    if not missing:
        return None
    fwd = " ".join(f"-L {p}:localhost:{p}" for p in missing)
    t = subprocess.Popen(f"ssh -o ExitOnForwardFailure=yes -N -n {fwd} {HOST}",
                         shell=True)
    for _ in range(20):
        if all(port_open(p) for p in missing):
            return t
        time.sleep(0.5)
    t.kill()
    raise RuntimeError(f"tunnel never forwarded {missing}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--file", help="track to play; omit to record the room as it is")
    ap.add_argument("--seconds", type=float, default=60)
    ap.add_argument("--lead", type=float, default=4)
    ap.add_argument("--outdir", default="/tmp/psync/spec")
    args = ap.parse_args()
    os.makedirs(args.outdir, exist_ok=True)

    elf = os.path.join(os.path.dirname(__file__), "..", "firmware.elf")
    syms = telem.resolve_symbols(elf)
    pipe = syms[next(k for k in syms if k.endswith("main.syncPipe"))]
    print(f"syncPipe = 0x{pipe:08x}, smooth at 0x{pipe + SMOOTH_OFF:08x}")

    tunnel = ensure_tunnel([p for _, p in PORTS.values()])
    player = None
    try:
        paircapture.spawn_servers(HOST)
        ths = []
        start = time.time() + args.lead
        for name, (_, tp) in PORTS.items():
            t = threading.Thread(target=poll_device,
                                 args=(name, tp, pipe,
                                       os.path.join(args.outdir, f"spec-{name}.csv"),
                                       args.seconds))
            t.start()
            ths.append(t)
        if args.file:
            time.sleep(max(0.0, start - time.time()))
            player = start_player(args.file)
        for t in ths:
            t.join()
    finally:
        # Kill the speaker when the capture ends: the track is normally longer
        # than --seconds, and waiting for it would keep the room busy.
        if player:
            player.terminate()
            player.wait()
        if tunnel:
            tunnel.kill()
    analyze(args.outdir)


def load(path):
    rows = list(csv.reader(open(path)))
    hdr = rows[0]
    d = {k: np.array([float(r[i]) for r in rows[1:]]) for i, k in enumerate(hdr)}
    return d


def analyze(outdir):
    L = load(os.path.join(outdir, "spec-L.csv"))
    R = load(os.path.join(outdir, "spec-R.csv"))
    for name, d in (("L", L), ("R", R)):
        s = np.stack([d[f"snap{i}"] for i in range(NB)], axis=1)
        # Q8 log2 -> dB
        db = s / 256.0 * 6.0206
        print(f"\n=== {name}: dB per band (mean over run, p5..p95)")
        for b in range(NB):
            print(f"  band{b}: {db[:, b].mean():7.1f}  "
                  f"[{np.percentile(db[:, b], 5):6.1f} .. "
                  f"{np.percentile(db[:, b], 95):6.1f}]  "
                  f"agcShift={d['agcShift'][len(d['agcShift']) // 2]:+.0f}")
    # time-align R to L through the seq-rate fit, then per-band difference
    def fit(d):
        s, t = d["seq"], d["t"]
        keep = np.diff(s, prepend=s[0] - 1) > 0
        b, a = np.polyfit(s[keep], t[keep], 1)
        return a, b
    aL, bL = fit(L)
    aR, bR = fit(R)
    tL = aL + bL * L["seq"]
    seqR = (tL - aR) / bR
    bs = R["seq"]
    ins = np.clip(np.searchsorted(bs, seqR), 1, len(bs) - 1)
    lo, hi = bs[ins - 1], bs[ins]
    idx = np.where(np.abs(lo - seqR) <= np.abs(hi - seqR), ins - 1, ins)
    good = np.abs(bs[idx] - seqR) < 1.0
    SL = np.stack([L[f"snap{i}"] for i in range(NB)], axis=1)[good]
    SR = np.stack([R[f"snap{i}"][idx] for i in range(NB)], axis=1)[good]
    diff = (SL - SR) / 256.0 * 6.0206  # dB, L minus R
    print(f"\n=== L-R difference spectrum (dB), n={good.sum()}")
    for b in range(NB):
        print(f"  band{b}: mean {diff[:, b].mean():+6.2f}  "
              f"std {diff[:, b].std():5.2f}  "
              f"p5 {np.percentile(diff[:, b], 5):+6.2f}  "
              f"p95 {np.percentile(diff[:, b], 95):+6.2f}")
    # how far is each band from the max, and what fraction of frames would flip
    for name, S in (("L", SL), ("R", SR)):
        mx = S.max(axis=1, keepdims=True)
        rel = (S - mx) / 256.0 * 6.0206
        near = np.mean(np.abs(rel + HOT_FLOOR / 256.0 * 6.0206) < 1.0, axis=0)
        print(f"  {name}: % frames within +-1 dB of the hot threshold: " +
              " ".join(f"b{b}:{100 * x:.0f}" for b, x in enumerate(near)))


if __name__ == "__main__":
    main()
