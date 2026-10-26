#!/usr/bin/env python3
"""Check that bitbang_update_bitplane matches the four per-group versions.

The bitplane builders in bitbang.h are inline assembly, so the host tests never
run them. This preprocesses bitbang.h and the four per-group versions it
replaced (tools/bitbang_ref.h), pulls each function's instruction string out,
emulates the few Thumb instructions they use (lsls/lsrs/rors/adcs/eors/
mvns/str) and compares the three stored words for random colours, with the
generic function's slot arguments permuted the way setLEDs permutes them.

  python3 tools/bitbang_equiv.py
"""

import os
import random
import re
import subprocess
import sys

M = 0xFFFFFFFF
here = os.path.dirname(os.path.abspath(__file__))
src = "".join(subprocess.run(["cpp", "-P", path], capture_output=True, text=True,
                             check=True).stdout
              for path in (os.path.join(here, "..", "bitbang.h"),
                           os.path.join(here, "bitbang_ref.h")))


def asm_of(name):
    m = re.search(r"\b%s\s*\(.*?__asm__\s+__volatile__\s*\((.*?)\n\s*:" % re.escape(name), src, re.S)
    if not m:
        sys.exit(f"{name} not found in bitbang.h")
    text = "".join(re.findall(r'"((?:[^"\\]|\\.)*)"', m.group(1)))
    return [i.strip() for i in text.replace("\\t", "").split("\\n") if i.strip()]


def run(prog, regs):
    regs = dict(regs)
    c = 0
    out = {}

    def val(op):
        op = op.strip()
        if op.startswith("#"):
            return int(op[1:], 0)
        return regs[re.fullmatch(r"%\[(\w+)\]", op).group(1)]

    for ins in prog:
        op, _, rest = ins.partition(" ")
        args = [a.strip() for a in re.split(r",(?![^\[]*\])", rest)]
        if op == "str":
            m = re.fullmatch(r"\[%\[(\w+)\](?:,\s*#(\d+))?\]", args[1])
            out[int(m.group(2) or 0)] = val(args[0])
            continue
        d = re.fullmatch(r"%\[(\w+)\]", args[0]).group(1)
        x = regs[d]
        if op == "lsls":
            n = val(args[1]) & 0xFF
            if n:
                c = (x >> (32 - n)) & 1 if n <= 32 else 0
                x = (x << n) & M
        elif op == "lsrs":
            n = val(args[1]) & 0xFF
            if n:
                c = (x >> (n - 1)) & 1 if n <= 32 else 0
                x = x >> n
        elif op == "rors":
            n = val(args[1]) & 0xFF
            if n:
                n %= 32
                x = ((x >> n) | (x << (32 - n))) & M
                c = x >> 31
        elif op == "adcs":
            s = val(args[1]) + val(args[2]) + c
            c = s >> 32
            x = s & M
        elif op == "eors":
            x ^= val(args[1])
        elif op == "mvns":
            x = ~val(args[1]) & M
        else:
            sys.exit(f"unhandled instruction {ins!r}")
        regs[d] = x
    return out


def main():
    generic = asm_of("bitbang_update_bitplane")
    groups = [asm_of(f"bitbang_update_bitplane_{g}") for g in (1, 2, 3, 4)]
    # setLEDs: which LED goes to slots a, b, c, d for each group (None = off).
    slots = [(0, None, 2, 1), (0, 2, None, 1), (0, 2, 1, None), (None, 2, 1, 0)]
    rng = random.Random(1)
    n = 0
    for trial in range(20000):
        leds = [rng.getrandbits(24) for _ in range(3)]
        if trial < 64:
            leds = [rng.choice([0, 0xFFFFFF, 0x080808, 0xF8F8F8]) for _ in range(3)]
        tmp = rng.getrandbits(32)
        for g in range(4):
            want = run(groups[g], {"tmp": tmp, "led0": leds[0], "led1": leds[1],
                                   "led2": leds[2], "N8": 8, "xor": 0b1000011111111111})
            arg = [0 if s is None else leds[s] for s in slots[g]]
            got = run(generic, {"tmp": tmp, "a": arg[0], "b": arg[1], "c": arg[2],
                                "d": arg[3], "N8": 8})
            if got != want:
                print(f"group {g + 1} leds={[hex(v) for v in leds]}:")
                for k in sorted(want):
                    print(f"  +{k}: want {want[k]:08x} got {got.get(k, 0):08x}")
                sys.exit(1)
            n += 1
    print(f"identical on {n} cases ({len(generic)} vs {sum(map(len, groups))} instructions)")


if __name__ == "__main__":
    main()
