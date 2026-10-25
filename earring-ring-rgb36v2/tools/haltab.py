#!/usr/bin/env python3
"""Is the LED refresh kicking the ADC? Halt the CPU and compare.

The excursions measured in the frozen windows are strictly one-sided (L: 50
samples more than 500 counts above the window median, 1 below; R: 104 and 0),
one sample wide, and up to +16381 counts - a quarter of full scale, and about
36 dB above the 1 kHz tone that is actually playing. Sound is symmetric, so this
is not the room.

Halving the possible causes does not need a rebuild: stopping the CPU with
`halt` stops the animation and the LED bit-bang, while TIM22, the ADC and DMA
keep running (they are peripherals, and the DMA needs no CPU). So:

  running  - normal firmware, LEDs being refreshed
  halted   - same analog state, no bit-bang at all

If the spikes collapse in the halted column, they come from the firmware's own
LED traffic, and no amount of audio-side filtering is a real fix - the drive
has to move out of the conversion windows. If they survive, the converter or its
bias network is making them and the pipeline's median filter is the fix.

  FLASH_HOST=localhost python3 tools/haltab.py --windows 20
"""
import argparse
import os
import struct
import sys
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem  # noqa: E402

TIM22 = 0x40011400
TIM_CR1 = TIM22 + 0x00
N = 250


def window(o, base):
    return np.array(struct.unpack("<%dH" % N,
                                  struct.pack("<%dI" % (N // 2), *o.mdw(base, N // 2))))


def sample(o, base, frozen):
    if frozen:
        o.cmd(f"mww 0x{TIM_CR1:08x} 0")
    x = window(o, base)
    if frozen:
        o.cmd(f"mww 0x{TIM_CR1:08x} 1")
    return x


def report(label, xs):
    devs = np.concatenate([x - np.median(x) for x in xs])
    up = int((devs > 500).sum())
    dn = int((devs < -500).sum())
    print(f"  {label:9s} n={len(xs):3d} windows  p-p med {np.median([x.max() - x.min() for x in xs]):7.0f}"
          f"  max {max(x.max() - x.min() for x in xs):7.0f}"
          f"  >500 up {up:5d}  down {dn:4d}  biggest {devs.max():+7.0f}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default=os.environ.get("FLASH_HOST", "pi"))
    ap.add_argument("--elf", default="firmware.elf")
    ap.add_argument("--windows", type=int, default=16)
    a = ap.parse_args()
    base = telem.resolve_symbols(a.elf)["main.adcData"]
    devs = [(n, telem.OpenOCD(a.host, p)) for p, n in ((4445, "L"), (4446, "R"))]
    run, halt = {n: [] for n, _ in devs}, {n: [] for n, _ in devs}
    # A halted earring looks dead to everything else on the rig, so the resume is
    # in a finally: an exception half-way must not leave a target stopped.
    try:
        for w in range(a.windows):
            for n, o in devs:                   # CPU running
                run[n].append(sample(o, base, True))
            time.sleep(0.25)
            for n, o in devs:                   # stop the CPU
                o.cmd("halt")
            time.sleep(0.3)
            for n, o in devs:
                halt[n].append(sample(o, base, True))
            for n, o in devs:
                o.cmd("resume")
            time.sleep(0.25)
    finally:
        for n, o in devs:
            try:
                o.cmd("resume")
            except Exception:  # noqa: BLE001
                pass
    print("excursions vs the window median (500-count gate):")
    for n, _ in devs:
        report(f"{n} running", run[n])
        report(f"{n} halted", halt[n])
    for n, o in devs:
        o.close()


main()
