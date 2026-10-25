#!/usr/bin/env python3
"""Positive controls for the capture chain, driven entirely over SWD.

Every "the microphone is dead" measurement so far has been a negative result on a
path that nobody had ever proven could produce a positive one. This puts two
signals into the same ADC that are known-good by construction, and takes one
away:

  VREFINT   channel 17, the bandgap. It is inside the chip, cannot fall off the
            board, and its factory value is read out of the system memory block,
            so the expected code is calculable. If the ADC, the oversampler, the
            DMA and this tool's read path are all fine, this reads it.
  mic off   PB1 is the microphone's supply. Pulling it low is a free way to
            remove the acoustic input completely: if PB0 is really the microphone
            output, the DC level and the noise must change when it is unpowered
            (the pin is left with nothing to hold it). If turning the microphone
            off changes nothing at all, PB0 is not looking at the microphone.

Registers are written directly and restored at the end. The CPU keeps running
throughout; nothing is flashed. This does write ADC_CCR/CHSELR/GPIOB_ODR, so do
not run it while a capture you care about is being recorded.

  python3 tools/pb0probe.py --ports 4445 4446 --names L R
"""

import argparse
import os
import struct
import sys
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem  # noqa: E402

HOST = os.environ.get("FLASH_HOST", "localhost")
ADC = 0x40012400
GPIOB = 0x50000400
VREFINT_CAL = 0x1FF80078
WINDOW = 250  # syncWindowSize: the circular DMA ring in adcData


def read_window(ocd, base):
    words = ocd.mdw(base, WINDOW // 2)
    w = np.array(words, dtype=np.uint32)
    x = np.empty(WINDOW, dtype=np.uint16)
    x[0::2] = (w & 0xFFFF).astype(np.uint16)
    x[1::2] = (w >> 16).astype(np.uint16)
    return x.astype(np.float64)


def stats(x):
    return dict(mean=float(x.mean()), std=float(x.std()), pp=float(x.max() - x.min()),
                lo=float(x.min()), hi=float(x.max()),
                # The mode is what a stuck or quantised reading shows up on.
                mode=float(np.bincount(x.astype(np.int64)).argmax()))


def show(label, s):
    print(f"  {label:<22} mean={s['mean']:9.1f} std={s['std']:7.2f} "
          f"p-p={s['pp']:8.1f} min={s['lo']:7.0f} max={s['hi']:7.0f} "
          f"modal={s['mode']:7.0f}")


def probe(ocd, adcdata, vref_cal):
    cr = ocd.mdw(ADC + 0x08, 1)[0]
    cfgr2 = ocd.mdw(ADC + 0x10, 1)[0]
    chselr = ocd.mdw(ADC + 0x28, 1)[0]
    ccr = ocd.mdw(ADC + 0x308, 1)[0]
    odr = ocd.mdw(GPIOB + 0x14, 1)[0]
    print(f"  CR={cr:#010x} CFGR2={cfgr2:#010x} CHSELR={chselr:#010x} "
          f"CCR={ccr:#010x} GPIOB_ODR={odr:#010x}")
    ovse = cfgr2 & 1
    osr = (1 << ((cfgr2 >> 2) & 0xF)) if ovse else 1
    shift = (cfgr2 >> 5) & 0xF
    # With oversampling the code is (sum of osr 12-bit samples) >> shift.
    k = osr / (1 << shift)
    print(f"  oversample {osr}x >> {shift}: one 12-bit LSB = {k:.1f} counts; "
          f"full scale 12-bit = {k * 4095:.0f}")

    show("mic on, ch8 (baseline)", stats(read_window(ocd, adcdata)))

    # --- VREFINT: a signal that cannot be disconnected -----------------------
    ocd.cmd(f"mww 0x{ADC + 0x308:08x} {ccr | (1 << 22):#x}")  # VREFEN
    time.sleep(0.01)  # bandgap startup is ~2 ms; the DMA window is 15.6 ms
    ocd.cmd(f"mww 0x{ADC + 0x28:08x} {1 << 17:#x}")
    time.sleep(0.06)
    s = stats(read_window(ocd, adcdata))
    show("VREFINT (ch17)", s)
    code12 = s["mean"] / k
    if vref_cal:
        vdda = 3.0 * vref_cal / max(code12, 1)
        print(f"  -> {code12:.0f} counts (12-bit); factory VREFINT_CAL={vref_cal} "
              f"at VDDA=3.00 V  =>  VDDA = {vdda:.2f} V, "
              f"VREFINT = {vdda * code12 / 4095:.3f} V")
    ocd.cmd(f"mww 0x{ADC + 0x28:08x} {chselr:#x}")
    ocd.cmd(f"mww 0x{ADC + 0x308:08x} {ccr:#x}")
    time.sleep(0.03)

    # --- the same pin with the microphone unpowered ---------------------------
    ocd.cmd(f"mww 0x{GPIOB + 0x14:08x} {odr & ~2:#x}")  # PB1 low: mic off
    time.sleep(0.03)
    show("mic power OFF, ch8", stats(read_window(ocd, adcdata)))
    show("mic power OFF again", stats(read_window(ocd, adcdata)))
    ocd.cmd(f"mww 0x{GPIOB + 0x14:08x} {odr:#x}")  # restore
    time.sleep(0.05)
    show("mic on again, ch8", stats(read_window(ocd, adcdata)))

    # --- is the DMA still running? -------------------------------------------
    n1 = ocd.mdw(0x4002000C, 1)[0]
    time.sleep(0.05)
    n2 = ocd.mdw(0x4002000C, 1)[0]
    d = (n1 - n2) % 250
    print(f"  DMA CNDTR {n1} -> {n2}: {d} samples in 50 ms "
          f"= {d / 0.05:.0f} SPS" + ("" if d else "   <-- DMA STOPPED"))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ports", type=int, nargs="+", default=[4445, 4446])
    ap.add_argument("--names", nargs="+", default=None)
    ap.add_argument("--elf", default="firmware.elf")
    a = ap.parse_args()
    names = a.names or [f"p{p}" for p in a.ports]
    sym = telem.resolve_symbols(a.elf)
    adcdata = sym["main.adcData"]
    for name, port in zip(names, a.ports):
        ocd = telem.OpenOCD(HOST, port)
        cal = ocd.mdh(VREFINT_CAL, 1) if hasattr(ocd, "mdh") else None
        vref = None
        if cal:
            vref = cal[0]
        else:
            w = ocd.mdw(VREFINT_CAL & ~3, 1)[0]
            vref = (w >> ((VREFINT_CAL & 2) * 8)) & 0xFFFF
        print(f"\n{name} @ {HOST}:{port}  adcData=0x{adcdata:08x}")
        probe(ocd, adcdata, vref)
        ocd.close()


main()
