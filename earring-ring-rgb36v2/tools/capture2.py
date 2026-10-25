#!/usr/bin/env python3
"""Capture telemetry AND a mic-envelope timeline in one poll loop.

Usage: capture2.py --play SS:DUR --out RUN.csv [--seconds 63]

Same telemetry polling as telem.py, plus one 250-sample ADC-buffer read per
iteration producing (t, seq, rms, floor) rows. The mic envelope is aligned to
the WAV's loudness envelope afterwards (align.py) to get the exact wall-clock
and device-seq position of music frame 0 — which the telemetry itself cannot
give (the pipeline's AGC'd `total` and coarse hash are too repetitive to
locate playback in a 57-minute podcast).

CSV rows (one file, two record kinds prefixed by a 'kind' column):
  kind=T  telemetry record (same fields as telem.py)
  kind=M  mic envelope sample: t, seq, rms
"""
import argparse
import os
import subprocess
import sys
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--play", metavar="SS:DUR")
    ap.add_argument("--file", help="play this WAV file instead of a podcast segment")
    ap.add_argument("--seconds", type=float)
    ap.add_argument("--podcast", default=telem.PODCAST_DEFAULT)
    ap.add_argument("--out", required=True)
    ap.add_argument("--elf", default=None)
    args = ap.parse_args()

    here = os.path.dirname(os.path.abspath(__file__))
    elf = args.elf or os.path.join(here, "..", "firmware.elf")
    syms = telem.resolve_symbols(elf)
    taddr = syms[next(k for k in syms if k.endswith(".Telem"))]
    adc = syms[next(k for k in syms if k.endswith("main.adcData"))]

    telem.spawn_server(os.path.dirname(elf))
    ocd = telem.OpenOCD()
    # Freshly started OpenOCD servers answer mdw with nothing until the
    # target has gone through a halt/resume cycle (reset run alone is not
    # enough; observed on this board with OpenOCD 0.12).
    ocd.cmd("halt")
    time.sleep(0.05)
    ocd.cmd("resume")
    time.sleep(0.05)

    if args.file:
        argv = ["ffplay", "-v", "error", "-nodisp", "-autoexit", args.file]
        dur = args.seconds or 999
    else:
        ss, _, dur = args.play.partition(":")
        dur = float(dur or args.seconds or 60)
        argv = ["ffplay", "-v", "error", "-nodisp", "-autoexit",
                "-ss", ss, "-t", str(dur), args.podcast]
    dur = float(dur)
    seconds = args.seconds or (dur + 3)

    tele_rows = []
    mic_t, mic_s, mic_r = [], [], []
    t_end = time.time() + seconds
    prev_seq = None
    t0 = time.time()
    player = subprocess.Popen(argv,
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    while time.time() < t_end:
        ctrl = ocd.mdw(taddr, telem.CTRL_WORDS)
        seq = ctrl[telem.CTRL_SEQ]
        w = np.array(ocd.mdw(adc, 125), dtype=np.uint32)
        s = np.concatenate([(w & 0xFFFF), (w >> 16)]).astype(np.float64)
        s -= s.mean()
        now = time.time() - t0
        mic_t.append(now); mic_s.append(seq); mic_r.append(float(s.std()))
        if prev_seq is None or seq < prev_seq:
            prev_seq = seq
        elif seq != prev_seq:
            n = min(seq - prev_seq, telem.RECORDS)
            ring = ocd.mdw(taddr + 4 * telem.TELEMETRIES_OFFSET,
                           telem.RECORDS * 4)
            for i in range(n - 1, -1, -1):
                sq = seq - 1 - i
                rec = telem.decode(sq, ring[(sq % telem.RECORDS) * 4:
                                            (sq % telem.RECORDS) * 4 + 4])
                rec["t"] = round(now, 3)
                tele_rows.append(rec)
            prev_seq = seq
    player.wait()
    ocd.close()

    with open(args.out, "w", newline="") as f:
        import csv
        fields = ["kind", "seq", "t", "hash", "onset", "beat", "bar", "anchor",
                  "pchange", "locked", "silent", "pushed", "mark", "returned",
                  "pattern",
                  "votes", "period", "total", "phase", "onset_age", "settled",
                  "primed", "nov", "d2", "d4", "d8", "rms"]
        wr = csv.DictWriter(f, fieldnames=fields)
        wr.writeheader()
        for r in tele_rows:
            row = dict(kind="T", rms="")
            row.update(r)
            wr.writerow(row)
        for t, s_, r_ in zip(mic_t, mic_s, mic_r):
            wr.writerow(dict(kind="M", seq=s_, t=round(t, 4), rms=round(r_, 1)))
    n = len(mic_t)
    print(f"{args.out}: {len(tele_rows)} telemetry rows, {n} mic samples "
          f"({n/(time.time()-t0):.0f} Hz), "
          f"rms p5={np.percentile(mic_r,5):.0f} p95={np.percentile(mic_r,95):.0f}")


if __name__ == "__main__":
    main()
