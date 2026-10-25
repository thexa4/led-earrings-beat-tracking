#!/usr/bin/env python3
"""Dump the whole analogue/clock register state of a running earring.

Every conclusion about "the microphone is dead" has to start here, because the
registers say what the ADC was actually told to do, as opposed to what the Go
source appears to tell it. Reading them is non-intrusive (a debug read does not
stop the CPU) and costs about a second per device.

  python3 tools/adcregs.py --ports 4445 4446 --names L R

The interesting fields, from RM0453:

  ADC_SMPR  SMP[2:0]     acquisition time, 0 = 1.5 ADC cycles ... 7 = 160.5
  ADC_CFGR2 OVSE/OVSR/OVSS  oversampling ratio and shift; CKMODE[1:0] at bit 30
  ADC_CCR   ADCSEL[1:0], ADCPRE[1:1], LFMEN  clock source, prescaler, low-power mode
  RCC_CFGR  SWS, HPRE, PPRE1, PPRE2  what the ADC and TIM22 are clocked from
  GPIOB_MODER bits 1:0 = PB0: 00 = analogue (what the ADC needs)
"""

import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem  # noqa: E402

HOST = os.environ.get("FLASH_HOST", "localhost")

# Peripheral bases, from TinyGo's device/stm32 (stm32l0x1.go), which is the
# same header the firmware compiles against.
ADC = 0x40012400
TIM22 = 0x40011400
DMA1 = 0x40020000
RCC = 0x40021000
PWR = 0x40007000
FLASH = 0x40022000
GPIOB = 0x50000400

# name, address, mask, decoder (a dict of value -> meaning) or a lambda
FIELDS = [
    ("ADC_ISR", ADC + 0x00, None, lambda v: {
        "ADRDRY": v >> 31 & 1, "OVR": v >> 29 & 1, "EOSMP": v >> 25 & 1,
        "EOC": v >> 2 & 1, "ADRDY": v >> 1 & 1}),
    ("ADC_CR", ADC + 0x08, None, lambda v: {
        "ADEN": v >> 0 & 1, "ADDIS": v >> 1 & 1, "ADSTART": v >> 2 & 1,
        "ADSTP": v >> 3 & 1, "DEEPPWD": v >> 29 & 1, "ADVREFEN": v >> 30 & 1}),
    ("ADC_CFGR1", ADC + 0x0C, None, lambda v: {
        "RES": (v >> 3) & 3, "DMAEN": v >> 8 & 1, "DMACFG": v >> 9 & 1,
        "AUTOFF": v >> 10 & 1, "SCAN": v >> 8 & 0, "EXTSEL": (v >> 6) & 7,
        "EXTEN": (v >> 10) & 3, "OVR": v >> 11 & 1, "CONT": v >> 13 & 1}),
    ("ADC_SMPR", ADC + 0x14, None, lambda v: {
        "SMP": v & 7,
        "cycles": [1.5, 3.5, 7.5, 12.5, 19.5, 39.5, 79.5, 160.5][v & 7]}),
    ("ADC_TR", ADC + 0x18, None, lambda v: {}),
    ("ADC_CHSELR", ADC + 0x28, None, lambda v: {"channel": v.bit_length() - 1,
                                                "raw": hex(v)}),
    ("ADC_DR", ADC + 0x40, None, lambda v: {"code": v}),
    ("ADC_CFGR2", ADC + 0x10, None, lambda v: {
        "OVSE": v >> 0 & 1,
        # OVSR is 3 bits at 4:2, encoded 0 = x2 ... 7 = x256.
        "OSR": (2 << ((v >> 2) & 7)) if v & 1 else 1,
        "OVSS": (v >> 5) & 0xF, "TOVS": v >> 9 & 1, "RESM": (v >> 11) & 3,
        "CKMODE": (v >> 30) & 3}),
    ("ADC_CCR", ADC + 0x308, None, lambda v: {
        "ADCSEL": v & 3, "ADCPRE": (v >> 28) & 3, "LFMEN": v >> 16 & 1,
        "VREFEN": v >> 10 & 1}),
    ("RCC_CR", RCC + 0x00, None, lambda v: {
        "MSION": v >> 0 & 1, "HSI16ON": v >> 8 & 1, "HSI16RDY": v >> 10 & 1,
        "HSEON": v >> 16 & 1, "PLLON": v >> 24 & 1}),
    ("RCC_ICSCR", RCC + 0x68, None, lambda v: {
        "MSIRANGE": (v >> 16) & 0xF}),
    ("RCC_CFGR", RCC + 0x08, None, lambda v: {
        "SW": v & 3, "SWS": (v >> 2) & 3, "HPRE": (v >> 4) & 0xF,
        "PPRE1": (v >> 8) & 7, "PPRE2": (v >> 11) & 7, "RTCSS": (v >> 8) & 0}),
    ("RCC_APB2ENR", RCC + 0x3C, None, lambda v: {
        "SYSCFG": v >> 0 & 1, "TIM22": v >> 2 & 1, "ADC": v >> 9 & 1,
        "TIM21": v >> 3 & 1}),
    ("PWR_CR", PWR + 0x00, None, lambda v: {
        "VOS": (v >> 9) & 3, "LPSDSR": v >> 1 & 1, "VOSF": v >> 16 & 1,
        "ULP": v >> 9 & 0}),
    ("FLASH_ACR", FLASH + 0x00, None, lambda v: {"LAT": v & 7}),
    ("TIM22_PSC", TIM22 + 0x28, None, lambda v: {"PSC": v}),
    ("TIM22_ARR", TIM22 + 0x2C, None, lambda v: {"ARR": v}),
    ("TIM22_CR1", TIM22 + 0x00, None, lambda v: {"CEN": v >> 0 & 1,
                                                 "ARPE": v >> 7 & 1}),
    ("TIM22_CR2", TIM22 + 0x18, None, lambda v: {"MMS": (v >> 4) & 7}),
    ("TIM22_DIER", TIM22 + 0x0C, None, lambda v: {"UDE": v >> 2 & 1}),
    ("GPIOB_MODER", GPIOB + 0x00, None, lambda v: {
        "PB0": ["analog", "input", "output", "altfunc"][v & 3],
        "PB1": ["analog", "input", "output", "altfunc"][v >> 2 & 3]}),
    ("GPIOB_PUPDR", GPIOB + 0x0C, None, lambda v: {
        "PB0": ["none", "up", "down"][v & 3], "PB1": ["none", "up", "down"][v >> 2 & 3]}),
    ("GPIOB_IDR", GPIOB + 0x10, None, lambda v: {"PB0": v >> 0 & 1,
                                                 "PB1": v >> 1 & 1}),
    ("GPIOB_ODR", GPIOB + 0x14, None, lambda v: {"PB1 (mic power)": v >> 1 & 1}),
    ("DMA_CCR", DMA1 + 0x08, None, lambda v: {
        "EN": v >> 0 & 1, "TEIE": v >> 1 & 1, "TCIE": v >> 2 & 1,
        "MINC": v >> 6 & 1, "PINC": v >> 7 & 1, "MSIZE": (v >> 10) & 3,
        "PSIZE": (v >> 8) & 3, "DIR": v >> 4 & 1, "CIRC": v >> 5 & 1,
        "PL": (v >> 12) & 3}),
    ("DMA_CNDTR", DMA1 + 0x0C, None, lambda v: {"NDTR": v}),
    ("DMA_PAR", DMA1 + 0x00, None, lambda v: {"PAR": hex(v)}),
    ("DMA_MAR", DMA1 + 0x04, None, lambda v: {"MAR": hex(v)}),
]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ports", type=int, nargs="+", default=[4445, 4446])
    ap.add_argument("--names", nargs="+", default=None)
    a = ap.parse_args()
    names = a.names or [f"p{p}" for p in a.ports]
    out = {}
    for name, port in zip(names, a.ports):
        ocd = telem.OpenOCD(HOST, port)
        vals = {}
        for reg, addr, _, _ in FIELDS:
            w = ocd.mdw(addr, 1)
            vals[reg] = w[0]
        ocd.close()
        out[name] = vals
    regs = [f for f in FIELDS if any(f[0] in v for v in out.values())]
    width = max(len(n) for n in names)
    print(f"{'field':<26}" + "".join(f"{n:>{width + 2}}" for n in names))
    for reg, _, _, dec in regs:
        cells = []
        for n in names:
            v = out[n].get(reg)
            cells.append("-" if v is None else str(dec(v) if dec else v))
        same = len(set(out[n].get(reg) for n in names if n in out)) <= 1
        print(f"{reg:<26}" + "".join(f"{c:>{width + 2}}" for c in cells)
              + ("" if same else "   <-- DIFFERS"))
    for n in names:
        v = out[n]
        adcclk = {0: "RC(MSI)", 1: "HSI16", 2: "LSI", 3: "SYSCLK"}[v["ADC_CCR"] & 3]
        pre = [1, 2, 4, 1][ (v["ADC_CCR"] >> 28) & 3]
        print(f"\n{n}: ADC clock = {adcclk}"
              + (f" / {pre}" if adcclk == "SYSCLK" else " (ADCPRE ignored unless SYSCLK)"))
        fs = 16e6 / (2 * (v["TIM22_PSC"] + 1)) if v["RCC_CFGR"] >> 2 & 3 == 3 else None
        if fs:
            cfgr2 = v["ADC_CFGR2"]
            osr = (2 << ((cfgr2 >> 2) & 7)) if cfgr2 & 1 else 1
            cyc = [1.5, 3.5, 7.5, 12.5, 19.5, 39.5, 79.5, 160.5][v["ADC_SMPR"] & 7] + 10.5
            # CCR PRESC (bits 21:18): 0 = /1, 1 = /2, 2 = /4, 3 = /6, 4 = /8, ...
            p = (v["ADC_CCR"] >> 18) & 0xF
            div = 1 if p == 0 else (2 if p == 1 else 2 * p)
            fadc = 16e6 / div
            us = osr * cyc / fadc * 1e6
            print(f"   trigger fs = {fs:.0f} Hz (period {1e6 / fs:.1f} us); oversample "
                  f"{osr}x shift {(cfgr2 >> 5) & 0xF}; a sample takes "
                  f"{osr * cyc:.0f} ADC cycles = {us:.1f} us at "
                  f"{fadc / 1e6:.1f} MHz")
            if us > 1e6 / fs:
                print("   ^ LONGER THAN THE TRIGGER PERIOD: the ADC misses "
                      "triggers, so the real sample rate is lower than fs")


main()
