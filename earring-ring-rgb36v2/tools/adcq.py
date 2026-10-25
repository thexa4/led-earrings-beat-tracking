#!/usr/bin/env python3
"""How much audio does the mic actually hand the ADC, vs. ADC acquisition time?

The sync capture path never writes ADC_SMPR, so the sample-and-hold gets its
reset value: 1.5 ADC clock cycles. The stock (non-sync) mic path runs the ADC
off an ~800 kHz PCLK, where 1.5 cycles is ~1.9 us; sync mode runs the ADC at
8 MHz, where the same setting is 190 ns. If the mic network cannot drive that,
the signal is attenuated by the RC and no amount of DSP helps.

Sweeps SMPR (and drops the oversampling ratio to buy the time back: 32x
oversampling eats 32 conversions per 62.5 us trigger period, so the current
config has almost nothing to spare) and measures, per burst of the 150 Hz ...
5 kHz fixture in /tmp/psync/tones.wav:

  ac    - coherent tone amplitude, in raw 12-bit ADC counts
  noise - same measurement one null off tone, in the silent gaps

Both earrings are measured at the same audio phase, and levels are divided by
the oversampling gain (N >> shift) so every config is in the same units.

Two traps this is careful about:
  * SMPR and CFGR2 are write-locked while ADEN=1. An earlier sweep polled
    `x & ADRDY == 0`, which Python parses as `x & (ADRDY == 0)`, so it never
    waited, every write was ignored, and all configs "measured" the same frozen
    buffer. Poll ADEN in CR for real here.
  * Reading the ring while DMA writes it tears the window. Freeze the channel
    (clear CCR.EN), read CNDTR to find the write position, rotate so samples
    are in time order, release.

Usage: tools/adcq.py [--seconds-only CONFIG_NAME]
"""

import argparse
import subprocess
import sys
import time

import numpy as np

sys.path.insert(0, __file__.rsplit("/", 1)[0])
import telem  # noqa: E402

ADC = 0x40012400
A_ISR, A_CR, A_SMPR, A_CFGR1, A_CFGR2, A_DR = 0x00, 0x08, 0x14, 0x0C, 0x10, 0x40
ADEN, ADDIS, ADSTART, ADRDY = 1, 2, 4, 1

DMA1 = 0x40020000
D_CCR, D_CNDTR = DMA1 + 0x08, DMA1 + 0x0C

RING = 250  # syncWindowSize; ADC ring at the base of adcData
FS = 16000  # nominal sample rate, TIM22 TRGO at PCLK/(2*(PSC+1))

TONES = [150, 400, 1000, 2500, 5000]
CYCLE = 16.0  # /tmp/psync/tones.wav: 1 s lead, then 2 s burst + 1 s gap x5
# Snapshot times inside the cycle: 3 mid-burst per tone, 4 in the gaps.
PHASES = [(1 + i * 3 + off, i) for i in range(5) for off in (0.4, 0.9, 1.4)]
PHASES += [(1 + i * 3 + 2.5, None) for i in range(4)]
PHASES.sort()

# (label, oversampling bits, shift, SMPR) - oversampling None disables it.
# OVS bits: 0..5 = 2,4,8,16,32,64. SMPR bits: 0..7 = 1.5,3.5,7.5,12.5,19.5,
# 39.5,79.5,160.5 cycles. Conversion cost = OVS * (SMP + 12.5) ADC cycles, and
# the trigger period is 62.5 us = 500 cycles at the 8 MHz ADC clock.
CONFIGS = [
    ("32x os  1.5c (as shipped)", 4, 1, 0),
    (" 1x     1.5c", None, 0, 0),
    (" 1x    19.5c", None, 0, 4),
    (" 1x    79.5c", None, 0, 6),
    (" 1x   160.5c", None, 0, 7),
    (" 2x os 160.5c", 0, 1, 7),
]


def ovs(bits):
    return 2 << bits if bits is not None else 1


class Dev:
    def __init__(self, name, port, ring_addr):
        self.name = name
        self.ocd = telem.OpenOCD("localhost", port)
        self.ring = ring_addr
        self.dmacr = None

    def rd(self, addr, count=1):
        for _ in range(5):
            try:
                return int(self.ocd.mdw(addr, count)[0])
            except Exception:
                time.sleep(0.05)
        raise RuntimeError(f"{self.name}: read 0x{addr:x} failed")

    def disable(self):
        self.ocd.cmd(f"mww 0x{ADC + A_CR:08x} {ADDIS}")
        for _ in range(100):
            if (self.rd(ADC + A_CR) & ADEN) == 0:
                return True
            time.sleep(0.01)
        return False

    def setup(self, ovs_bits, shift, smp):
        """Reconfigure; returns False if the ADC would not come back."""
        self.dmacr = self.rd(D_CCR)
        self.ocd.cmd(f"mww 0x{D_CCR:08x} {self.dmacr & ~1:#x}")  # stop DMA
        if not self.disable():
            return False
        self.ocd.cmd(f"mww 0x{ADC + A_SMPR:08x} {smp}")
        cfgr2 = 0 if ovs_bits is None else (1 | (ovs_bits << 2) | (shift << 5))
        self.ocd.cmd(f"mww 0x{ADC + A_CFGR2:08x} {cfgr2}")
        # Peripheral readback is unreliable while the part runs (GPIOB reads 0
        # on a device that is audibly hearing), so this is a note, not a check.
        back = self.rd(ADC + A_SMPR) & 7
        if back != smp:
            print(f"  {self.name}: SMPR reads {back}, wrote {smp}", flush=True)
        self.ocd.cmd(f"mww 0x{ADC + A_CR:08x} {ADEN}")
        for _ in range(100):
            if self.rd(ADC + A_ISR) & ADRDY:
                break
            time.sleep(0.01)
        else:
            return False
        self.ocd.cmd(f"mww 0x{ADC + A_CR:08x} {ADEN | ADSTART}")
        self.ocd.cmd(f"mww 0x{D_CCR:08x} {self.dmacr | 1:#x}")
        return True

    def running(self):
        """Samples/s the DMA is actually taking, from CNDTR advance."""
        a, t1 = self.rd(D_CNDTR), time.time()
        time.sleep(0.01)
        b = time.time(), self.rd(D_CNDTR)
        return (a - b[1]) % RING / (b[0] - t1)

    def snapshot(self):
        """One intact 250-sample window, in time order."""
        cr = self.rd(D_CCR)
        self.ocd.cmd(f"mww 0x{D_CCR:08x} {cr & ~1:#x}")
        head = self.rd(D_CNDTR)
        words = self.ocd.mdw(self.ring, RING // 2)
        self.ocd.cmd(f"mww 0x{D_CCR:08x} {cr | 1:#x}")
        s = np.empty(RING, dtype=np.uint16)
        s[0::2] = np.array(words, dtype=np.uint32) & 0xFFFF
        s[1::2] = np.array(words, dtype=np.uint32) >> 16
        return np.roll(s.astype(np.float64), -(RING - head) % RING)


HANN = np.hanning(RING)
T = np.arange(RING) / FS


def amp(x, freq):
    """Coherent projection: amplitude of `freq` in samples sampled at FS."""
    return 2 * abs(((x - x.mean()) * HANN) * np.exp(-2j * np.pi * freq * T)).sum() / HANN.sum()


def band(x, freq):
    """Peak over +-30%: clock error and a dropped-trigger sample rate show up
    as a frequency scale factor, not a level error. Clipped below Nyquist -
    projecting above it folds real noise back onto the tone."""
    grid = np.linspace(0.7, 1.3, 61)
    grid = grid[freq * grid < 0.95 * (FS / 2)]
    return max(amp(x, freq * k) for k in grid) if len(grid) else 0.0


def wait_until(t):
    while True:
        d = t - time.time()
        if d <= 0:
            return
        time.sleep(min(d, 0.05))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", help="run a single config by label substring")
    ap.add_argument("--elf", default="firmware.elf")
    args = ap.parse_args()

    syms = telem.resolve_symbols(args.elf)
    ring = syms[next(k for k in syms if k.endswith("main.adcData"))]
    print(f"adcData = 0x{ring:08x}")

    devs = [Dev("L", 4445, ring), Dev("R", 4446, ring)]
    configs = [c for c in CONFIGS
               if args.config is None or args.config.lower() in c[0].lower()]

    player = subprocess.Popen(["ffplay", "-v", "error", "-nodisp", "-autoexit",
                               "-loop", "40", "/tmp/psync/tones.wav"])
    time.sleep(0.5)
    t0 = time.time()

    hdr = f"{'config':26s} {'dev':4s} {'acq us':>7} | " + " ".join(
        f"{f:>6}Hz" for f in TONES) + " |  noise  std"
    for label, ovs_bits, shift, smp in configs:
        acq = [1.5, 3.5, 7.5, 12.5, 19.5, 39.5, 79.5, 160.5][smp]
        ok = all(d.setup(ovs_bits, shift, smp) for d in devs)
        # Align to the start of the next cycle so phases mean the same thing
        # for every config.
        n = int((time.time() - t0) / CYCLE) + 1
        wait_until(t0 + n * CYCLE + 0.1)
        acc = {d.name: {i: [] for i in range(5)} for d in devs}
        null = {d.name: [] for d in devs}
        std = {d.name: [] for d in devs}
        for ph, i in PHASES:
            wait_until(t0 + n * CYCLE + ph)
            for d in devs:
                x = d.snapshot()
                g = ovs(ovs_bits) >> shift  # codes per raw 12-bit count
                if i is None:
                    null[d.name].append(max(band(x, f * 1.37) for f in TONES) / g)
                else:
                    acc[d.name][i].append(band(x, TONES[i]) / g)
                    std[d.name].append((x.std()) / g)
        print(hdr)
        for d in devs:
            if not ok:
                print(f"{label:26s} {d.name:4s} ADC would not restart")
                continue
            print(f"{label:26s} {d.name:4s} {acq / 8:7.2f} | " + " ".join(
                f"{np.median(acc[d.name][i]):6.0f}" for i in range(5)) +
                f" | {np.median(null[d.name]):6.0f} {np.median(std[d.name]):5.0f}")

    player.kill()
    for d in devs:
        d.ocd.cmd("reset run")
        time.sleep(0.2)
        d.ocd.close()


if __name__ == "__main__":
    main()
