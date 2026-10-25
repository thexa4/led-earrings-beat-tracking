#!/usr/bin/env python3
"""Sweep the ADC acquisition time live and see whether the microphone answers.

SMPR decides how long the sample-and-hold capacitor is allowed to charge before a
conversion starts. The reset value is 1.5 ADC cycles: at the 16 MHz ADC clock this
firmware runs, that is 94 ns. The datasheet allows up to 160.5 cycles, and the
32x oversampling budget (62.5 us per sample at 16 kSPS = 1000 ADC cycles = 31 per
sub-conversion, of which the conversion itself takes 10.5) leaves room for about
20 cycles without dropping any sample rate at all.

If the readings get louder as SMP grows, the pin has been read before it settled
and everything measured on this channel so far - including "the microphones do not
hear the music" - was attenuated by an unknown, device-dependent factor. If the
readings do not budge, acquisition is not the problem and the channel really is
quiescent.

SMPR is write-locked while ADEN=1, so each step is a short teardown and restart:
ADDIS -> wait ADEN=0 -> write SMPR -> ADEN -> wait ADRDY -> ADSTART. The DMA is
left running throughout, so the ring keeps filling (with the stale DR value for
the ~1 ms the ADC is down, which is why the first window after each change is
discarded).

  python3 tools/smpsweep.py --ports 4445 4446 --names L R
"""

import argparse
import os
import statistics
import sys
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem  # noqa: E402

HOST = os.environ.get("FLASH_HOST", "localhost")
ADC = 0x40012400
ISR, CR, CFGR1, CFGR2, SMPR, CHSELR = 0x00, 0x08, 0x0C, 0x10, 0x14, 0x28
ADEN, ADDIS, ADSTART = 1 << 0, 1 << 1, 1 << 2
ADRDY = 1 << 0
WINDOW = 250
CYC = [1.5, 3.5, 7.5, 12.5, 19.5, 39.5, 79.5, 160.5]


def window(ocd, base):
    w = np.array(ocd.mdw(base, WINDOW // 2), dtype=np.uint32)
    x = np.empty(WINDOW, dtype=np.uint16)
    x[0::2] = (w & 0xFFFF).astype(np.uint16)
    x[1::2] = (w >> 16).astype(np.uint16)
    return x.astype(np.float64)


def set_smpr(ocd, s):
    """Re-enter the ADC with a new SMPR. Returns True if the ADC came back ready."""
    ocd.cmd(f"mww 0x{ADC + CR:08x} {ADDIS:#x}")
    for _ in range(50):
        if not (ocd.mdw(ADC + CR, 1)[0] & ADEN):
            break
        time.sleep(0.002)
    ocd.cmd(f"mww 0x{ADC + SMPR:08x} {s:#x}")
    ocd.cmd(f"mww 0x{ADC + CR:08x} {ADEN:#x}")
    ready = False
    for _ in range(50):
        if ocd.mdw(ADC + ISR, 1)[0] & ADRDY:
            ready = True
            break
        time.sleep(0.002)
    ocd.cmd(f"mww 0x{ADC + CR:08x} {ADEN | ADSTART:#x}")
    time.sleep(0.03)
    return ready


def measure(ocd, base, k, n=10):
    xs = []
    for i in range(n + 1):
        x = window(ocd, base)
        if i:  # drop the first: it straddles the restart
            xs.append(x)
    # The impulse noise on this channel is huge compared with the audio, so the
    # statistics that matter are the robust ones: the median absolute deviation
    # is the AC level of the bulk of the samples, the p-p is dominated by spikes.
    med = np.median([np.median(x) for x in xs])
    mad = np.median([1.4826 * np.median(np.abs(x - med)) for x in xs])
    pp = np.median([x.max() - x.min() for x in xs])
    dc = np.median([x.mean() for x in xs])
    frac = np.median([np.mean(np.abs(x - med) > 8 * mad + 1) for x in xs])
    return dc / k, mad / k, pp / k, frac


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ports", type=int, nargs="+", default=[4445, 4446])
    ap.add_argument("--names", nargs="+", default=None)
    ap.add_argument("--elf", default="firmware.elf")
    a = ap.parse_args()
    names = a.names or [f"p{p}" for p in a.ports]
    base = telem.resolve_symbols(a.elf)["main.adcData"]

    for name, port in zip(names, a.ports):
        ocd = telem.OpenOCD(HOST, port)
        cfgr2 = ocd.mdw(ADC + CFGR2, 1)[0]
        osr = (2 << ((cfgr2 >> 2) & 7)) if cfgr2 & 1 else 1
        shift = (cfgr2 >> 5) & 0xF
        k = osr / (1 << shift)  # one 12-bit LSB, in the codes the DMA stores
        orig = ocd.mdw(ADC + SMPR, 1)[0] & 7
        print(f"\n{name} @ {HOST}:{port}: oversample {osr}x>>{shift} "
              f"(1 LSB = {k:.0f} codes), SMPR was {orig}")
        print(f"{'SMP':>4} {'cycles':>7} {'us@16MHz':>9} {'fits?':>6} "
              f"{'DC':>9} {'MAD':>8} {'p-p':>9} {'spikes':>7} {'ADC ready':>10}")
        for s in range(8):
            budget = osr * (CYC[s] + 10.5) / 16e6 * 1e6
            fits = "yes" if budget <= 62.5 else "NO"
            ok = set_smpr(ocd, s)
            dc, mad, pp, frac = measure(ocd, base, k)
            print(f"{s:>4} {CYC[s]:>7.1f} {CYC[s] / 16 * 1e3:>9.0f} {fits:>6} "
                  f"{dc:>9.1f} {mad:>8.2f} {pp:>9.1f} {100 * frac:>6.2f}% "
                  f"{'ok' if ok else 'TIMEOUT':>10}")
        set_smpr(ocd, orig)
        print(f"  restored SMPR={orig}")
        ocd.close()
    print("\nMAD is the AC level of the bulk of the samples, in 12-bit LSB: that is")
    print("the number that would grow if the pin were being read too early.")


main()
