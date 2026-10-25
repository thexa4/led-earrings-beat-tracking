#!/usr/bin/env python3
"""Is there audible sound in the room, as the earrings measure it?

The question this answers is narrower and far more reliable than "do the two
earrings agree": does a pulse of sound sent to the speaker actually arrive at each
microphone? Every agreement number depends on it, and getting it wrong wastes whole
sessions - a capture where the room was silent looks exactly like a capture where the
algorithm is bad.

Why a pulsed tone, and why the input peak:

  pulsed tone   Five seconds on, five off, is a 0.1 Hz square wave. Nothing the AGC
                does, the beat tracker does, or the wearer does resembles it, so a
                response at that rate cannot be a false positive - and unlike music,
                the ground truth is known exactly with no clock alignment and no
                cross-device comparison involved. One side answering while the other
                does not localises the fault to that unit immediately.
  input peak    agcPeak is the peak the AGC measures on its OWN input, before any
                gain is applied. The two spectrum columns are both useless for this:
                the smoothed one is level-normalised by design, and the raw one is
                measured after the AGC gain, so it moves UP when the room goes quiet.
                Watching either of those calls a working earring deaf.

  tools/tonelisten.py [--tone WAV] [--seconds 45] [--lead 4]

Exit status is 0 when both sides answer the pulse. The tone file should be
silence/tone alternating in blocks of a few seconds; /tmp/psync/tonerpg.wav works.
"""
import argparse
import math
import os
import subprocess
import sys
import threading
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import specdump  # noqa: E402
import telem  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
PORTS = specdump.PORTS
NB = specdump.NB
AGC_PEAK = 59   # word index into the Pipeline block, as in specdump
AGC_SHIFT = 60
RAW0 = 16
SNAP0 = 43


def poll(name, tport, pipe, rows, stop):
    ocd = specdump.open_ocd(tport)
    try:
        while not stop.is_set():
            try:
                blk = ocd.mdw(pipe, 62)
            except Exception:
                time.sleep(0.2)
                continue
            rows.append([time.time(), specdump.sig(blk[AGC_PEAK]),
                         specdump.sig(blk[AGC_SHIFT])]
                        + [specdump.sig(blk[RAW0 + i]) for i in range(NB)]
                        + [specdump.sig(blk[SNAP0 + i]) for i in range(NB)])
            time.sleep(0.04)
    finally:
        ocd.close()


def tone_state(t, lead):
    """True during a tone burst, given the file's own 5 s on / 5 s off pattern."""
    u = t - lead
    return u >= 0 and int(u // 5) % 2 == 1


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tone", default="/tmp/psync/tonerpg.wav")
    ap.add_argument("--seconds", type=float, default=45)
    ap.add_argument("--lead", type=float, default=4)
    a = ap.parse_args()

    elf = os.path.join(HERE, "..", "firmware.elf")
    syms = telem.resolve_symbols(elf)
    pipe = syms[next(k for k in syms if k.endswith("main.syncPipe"))]

    threads, data, stop = [], {}, threading.Event()
    for name, (_, tp) in PORTS.items():
        data[name] = []
        t = threading.Thread(target=poll, args=(name, tp, pipe, data[name], stop))
        t.start()
        threads.append(t)
    time.sleep(max(0.0, a.lead - 1))
    print(f"playing {os.path.basename(a.tone)} for {a.seconds:.0f} s "
          f"(tone bursts every 10 s from t={a.lead:.0f} s)", flush=True)
    p = subprocess.Popen(["ffplay", "-v", "error", "-nodisp", "-autoexit",
                          "-t", str(a.seconds), a.tone],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(a.seconds)
    p.terminate()
    p.wait()
    stop.set()
    for t in threads:
        t.join()

    t0 = min(r[0] for r in data["L"] + data["R"])
    bad = []
    print(f"\n  unit  samples   input-peak doublings  tone-off -> tone-on"
          f"   verdict")
    for name in ("L", "R"):
        rows = data[name]
        if len(rows) < 50:
            print(f"  {name:4} {len(rows):7}   (too few samples to judge)")
            bad.append(name)
            continue
        best, blag = -99, 0
        for lag in range(0, 8):          # speaker latency, whole seconds
            on = [r[1] for r in rows if tone_state(r[0] - t0, a.lead - lag)
                  and r[1] > 511]
            off = [r[1] for r in rows if not tone_state(r[0] - t0, a.lead - lag)]
            if len(on) < 20 or len(off) < 20:
                continue
            med = math.log2(max(1.0, _median(on)) / max(1.0, _median(off)))
            if med > best:
                best, blag = med, lag
        print(f"  {name:4} {len(rows):7}   {best:+11.2f}"
              f"            (best at {blag} s latency)", end="")
        if best >= 1.0:
            print("   HEARS the tone")
        else:
            print("   silent")
            bad.append(name)
    print()
    if bad:
        print(f"  NO TONE REACHING: {' and '.join(bad)}")
        print("  The input peak does not rise during the burst, so this unit is not")
        print("  measuring the speaker. Until it does, no pair measurement means")
        print("  anything: check the sink ffplay picked (wpctl status), that the")
        print("  speaker is actually on and audible, and the unit's placement.")
        return 1
    print("  both microphones pick up the speaker; the bench is usable.")
    return 0


def _median(v):
    v = sorted(v)
    return v[len(v) // 2] if v else 0.0


if __name__ == "__main__":
    sys.exit(main())
