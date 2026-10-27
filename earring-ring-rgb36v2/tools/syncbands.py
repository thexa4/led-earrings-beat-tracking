#!/usr/bin/env python3
"""Record the beat comb's input on both earrings, and replay the comb on it.

  tools/syncbands.py record OUTDIR --elf syncboot.elf --seconds 330 \\
      [--play podcast.mp3 --ss 2400]
  tools/syncbands.py replay OUTDIR WAV BEATS SS [--bands 2,3 ...]

record polls syncdsp.Pipeline inside main.syncPipe (address from --elf, the
exact image on the earrings; field offsets from its DWARF): lvlHist - this
and the previous frame's log2 band energies, gain-corrected - and beatFlux
every read, the comb state (Locked, Period, perQ8, pos) every 8th. ~100 reads/s
per earring, so nearly every 64/s frame is caught, and a missed one comes
back from the next read's lvlHist[1]. One ffplay plays here; the volume is
never touched. Writes OUTDIR/L.npy, R.npy and play.txt.

replay rebuilds each earring's frames, finds the latency by correlating the
<250 Hz band with the WAV (as synckick.py does), and runs Tick's comb on the
positive two-frame flux of each --bands set (pipeline band index, 0 = >4 kHz
.. 7 = <250 Hz, 8 = total), with each earring's own recorded period. Per set:
beats within 70 ms of a kick, both earrings' beats within 32 ms, and the share
of L's beats half a beat (bins 3-5 of 8) from R's. Beats at a tempo more than
4% off the kicks' are left out, so this scores the phase choice alone. The
comb itself does not feed back into what the earrings hear, so the replay
scores any flux rule on what the bench microphones really heard
(PARTY-SYNC.md, Stage 5).

WAV/BEATS as for synckick.py: a 16 kHz excerpt starting at podcast second SS
and its kick reference from tools/beatref/kbeats.py.
"""
import argparse
import os
import re
import struct
import subprocess
import sys
import threading
import time
import wave

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem

PORTS = {"L": 4445, "R": 4446}
FR = 64.0
# syncdsp.Pipeline: beat (BeatTracker: Locked 0, Period 4, ... perQ8 188, pos
# 192), lvlHist [2][9]int16, beatFlux, lvlOff, frame. Checked against --elf.
OFF_BEAT, OFF_PERQ8, OFF_LVL = 300, 300 + 188, 1160  # OFF_LVL: word holding lvlHist[0][0]
HEX = re.compile(r"(?<![0-9a-fA-Fx])[0-9a-fA-F]{8}(?![0-9a-fA-F])")


def pipe_addr(elf):
    syms = subprocess.run(["readelf", "-sW", elf], capture_output=True, text=True).stdout
    addr = [int(l.split()[1], 16) for l in syms.splitlines() if l.endswith(" main.syncPipe")]
    info = subprocess.run(["readelf", "--debug-dump=info", elf], capture_output=True, text=True).stdout
    loc = {}
    for m in re.finditer(r"DW_AT_name\s*:.*: (lvlHist|perQ8)\n(?:.*\n){0,3}?.*DW_AT_data_member_location: (\d+)", info):
        loc[m.group(1)] = int(m.group(2))
    if loc != {"lvlHist": OFF_LVL + 2, "perQ8": 188} or len(addr) != 1:
        sys.exit(f"Pipeline layout changed ({loc}); update the offsets in {__file__}")
    return addr[0]


def poll(side, base, stop, rows, errs):
    lvl = f"mdw {base + OFF_LVL:#x} 13"
    full = f"echo [mdw {base + OFF_BEAT:#x} 2][mdw {base + OFF_PERQ8:#x} 2][{lvl}]"
    o = telem.OpenOCD("localhost", PORTS[side])
    beat, i = [0] * 4, 0
    while not stop.is_set():
        i += 1
        isfull = i % 8 == 0  # the comb state changes slowly, the bands every frame
        t0 = time.time()
        try:
            txt = o.cmd(full if isfull else lvl)
        except Exception:
            errs[side] += 1
            time.sleep(0.05)
            try:
                o = telem.OpenOCD("localhost", PORTS[side])
            except Exception:
                pass
            continue
        t1 = time.time()
        w = [int(x, 16) for x in HEX.findall(txt.split("\n", 1)[-1] if txt.startswith("echo") else txt)]
        if len(w) != (17 if isfull else 13):
            errs[side] += 1
            continue
        if isfull:
            beat, w = w[:4], w[4:]
        rows.append([t0, t1, isfull] + beat + w)


def record(a):
    base = pipe_addr(a.elf)
    os.makedirs(a.outdir, exist_ok=True)
    stop, errs, rows = threading.Event(), {s: 0 for s in PORTS}, {s: [] for s in PORTS}
    th = [threading.Thread(target=poll, args=(s, base, stop, rows[s], errs)) for s in PORTS]
    for t in th:
        t.start()
    player = None
    if a.play:
        player = subprocess.Popen(["ffplay", "-v", "error", "-nodisp", "-autoexit", "-ss", str(a.ss),
                                   "-t", str(a.seconds), a.play], stdin=subprocess.DEVNULL)
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
    for s in PORTS:
        np.save(f"{a.outdir}/{s}.npy", np.array(rows[s], dtype=np.float64))
        print(f"{s}: {len(rows[s]) / a.seconds:.0f} reads/s")
    print("read errors:", errs)


def frames(path):
    """Per frame: V (9 log2 band energies), T (host time), B (Locked, Period,
    perQ8, pos), from one earring's reads."""
    a = np.load(path)
    t = (a[:, 0] + a[:, 1]) / 2
    raw = [struct.pack("<13I", *r) for r in a[:, 7:20].astype(np.int64)]
    h0 = np.array([struct.unpack_from("<9h", r, 2) for r in raw])
    h1 = np.array([struct.unpack_from("<9h", r, 20) for r in raw])
    fr = a[:, 19].astype(np.int64)
    # lvlHist is written before the frame counter moves on, so a read can see
    # this frame's bands with last frame's count: label each distinct
    # lvlHist[0] with the highest count seen with it.
    lab = {}
    for i in range(len(t)):
        k = h0[i].tobytes()
        if k not in lab or fr[i] - 1 > lab[k][0]:
            lab[k] = (fr[i] - 1, i)
    items = sorted(lab.values())
    f0 = items[0][0]
    n = items[-1][0] - f0 + 1
    V, T = np.full((n, 9), np.nan), np.full(n, np.nan)
    for f, i in items:
        V[f - f0], T[f - f0] = h0[i], t[i]
        if f > f0 and np.isnan(V[f - 1 - f0, 0]):
            V[f - 1 - f0] = h1[i]
    lost = int(np.isnan(V[:, 0]).sum())
    for j in range(1, n):
        if np.isnan(V[j, 0]):
            V[j] = V[j - 1]
    ok, fi = ~np.isnan(T), np.arange(n)
    slope, icept = np.polyfit(fi[ok], T[ok], 1)
    full = [(fr[i] - 1 - f0, a[i, 3:7].astype(np.int64)) for i in range(len(t)) if a[i, 2]]
    B, k = np.zeros((n, 4), np.int64), 0
    for j in range(n):
        while k + 1 < len(full) and full[k + 1][0] <= j:
            k += 1
        B[j] = full[k][1]
    return dict(V=V, T=icept + slope * fi, B=B, lost=lost)


def comb(x, B):
    """BeatTracker.Tick's comb on per-frame flux x. Returns beat frames."""
    c, pos, best, since, beats = np.zeros(64), None, 0, 0, []
    for j in range(len(x)):
        locked, period, perq8 = B[j, :3]
        if not locked:
            continue
        if pos is None:
            pos = int(B[j, 3])
        pos += 256
        while pos >= perq8:
            pos -= perq8
        b = pos >> 8
        c[b] += (x[j] - c[b]) / 8  # BeatCombShift = 3
        if c[b] > c[best]:
            best = b
        since += 1
        if b == best and since * 2 >= period:
            since = 0
            beats.append(j)
    return np.array(beats, int)


def wav_env(path):
    w = wave.open(path)
    sr = w.getframerate()
    x = np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16).astype(float)
    hop = int(sr / FR)
    n = len(x) // hop
    X = np.fft.rfft(x[: n * hop].reshape(n, hop) * np.hanning(hop), axis=1)
    f = np.fft.rfftfreq(hop, 1 / sr)
    return np.log2((np.abs(X[:, (f > 30) & (f < 250)]) ** 2).sum(1) + 1e3)


def latency(d, t_play, off, env):
    dev = d["V"][:, 7] - np.convolve(d["V"][:, 7], np.ones(64) / 64, "same")
    best = (-2, 0)
    for lat in np.arange(0.0, 6.0, 1 / FR / 2):
        k = np.round((d["T"] - t_play - lat + off) * FR).astype(int)
        ok = (k >= 0) & (k < len(env)) & (d["T"] > d["T"][0] + 20)
        e = env[k[ok]] - np.convolve(env[k[ok]], np.ones(64) / 64, "same")
        best = max(best, (np.corrcoef(dev[ok], e)[0, 1], lat))
    return best


def replay(a):
    t_play, _, ss = open(f"{a.outdir}/play.txt").read().split()
    t_play, ss = float(t_play), float(ss.split("=")[1])
    kicks = np.loadtxt(a.beats)
    kp = np.median(np.diff(kicks))
    env = wav_env(a.wav)
    E = {}
    for s in PORTS:
        d = frames(f"{a.outdir}/{s}.npy")
        c, lat = latency(d, t_play, ss - a.ss, env)
        d["A"] = d["T"] - t_play - lat + (ss - a.ss)  # WAV time of each frame
        D = np.zeros_like(d["V"])
        D[2:] = np.maximum(d["V"][2:] - d["V"][:-2], 0)
        d["D"] = D
        rate = 1 / np.polyfit(np.arange(len(d["T"])), d["T"], 1)[0]
        d["right"] = np.abs(d["B"][:, 2] / 256 / (kp * rate) - 1) < 0.04
        E[s] = d
        print(f"{s}: latency {lat * 1e3:.0f} ms (corr {c:.2f}), frames lost {d['lost']}, "
              f"at the kicks' tempo {d['right'].mean():.0%}")
    for spec in a.bands:
        sel = [int(b) for b in spec.split(",")]
        bt, hit = {}, {}
        for s, d in E.items():
            b = comb(d["D"][:, sel].sum(1), d["B"])
            b = b[d["right"][b]]
            bt[s] = d["T"][b]
            at = d["A"][b]
            at = at[(at > kicks[0] + 20) & (at < kicks[-1])]
            j = np.clip(np.searchsorted(kicks, at), 1, len(kicks) - 1)
            hit[s] = np.mean(np.minimum(at - kicks[j - 1], kicks[j] - at) < 0.07)
        tl, tr = bt["L"], bt["R"]
        tl = tl[tl > max(tl[0], tr[0]) + 20]
        j = np.clip(np.searchsorted(tr, tl), 1, len(tr) - 1)
        near = np.minimum(np.abs(tl - tr[j - 1]), np.abs(tr[j] - tl)) < 0.032
        gap = tr[j] - tr[j - 1]
        ph = ((tl - tr[j - 1]) / gap * 8 + 0.5) % 8
        half = np.mean((ph[gap < 0.6] >= 3) & (ph[gap < 0.6] < 6))
        print(f"bands {spec:12s} kick L {hit['L']:.0%} R {hit['R']:.0%}   "
              f"within 32 ms {near.mean():.0%}   half a beat apart {half:.0%}")


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("record")
    r.add_argument("outdir")
    r.add_argument("--elf", required=True)
    r.add_argument("--seconds", type=float, default=330)
    r.add_argument("--play")
    r.add_argument("--ss", type=float, default=0)
    p = sub.add_parser("replay")
    p.add_argument("outdir")
    p.add_argument("wav")
    p.add_argument("beats")
    p.add_argument("ss", type=float)
    p.add_argument("--bands", nargs="+", default=["0,1,2,3,4,5,6,7,8", "2,3"])
    a = ap.parse_args()
    record(a) if a.cmd == "record" else replay(a)


if __name__ == "__main__":
    main()
