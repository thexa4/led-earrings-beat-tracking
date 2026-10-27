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


def asm_blocks(name):
    """The instructions of each asm statement in function name, in order."""
    m = re.search(r"\b%s\s*\((.*?)\n}" % re.escape(name), src, re.S)
    if not m:
        sys.exit(f"{name} not found in bitbang.h")
    blocks = []
    for body in re.findall(r"__asm__\s+__volatile__\s*\((.*?)\s*:", m.group(1), re.S):
        text = "".join(re.findall(r'"((?:[^"\\]|\\.)*)"', body))
        blocks.append([i.strip() for i in text.replace("\\t", "").split("\\n") if i.strip()])
    return blocks


def asm_of(name):
    return asm_blocks(name)[0]


def generic_prog():
    """bitbang_update_bitplane as straight-line code: the first bitplane, then
    the loop's four (the C stores a word after every one but the second)."""
    first, plane = asm_blocks("bitbang_update_bitplane")
    prog = list(first)
    for i in range(4):
        prog += plane
        if i != 1:
            prog.append("str %%[tmp], [%%[bitplane], #%d]" % (4 * (i - (i > 1))))
    return prog, len(first) + len(plane)


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
    generic, ninsns = generic_prog()
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
            # The C inverts the slots and clears tmp before the asm.
            got = run(generic, {"tmp": 0, "a": ~arg[0] & M, "b": ~arg[1] & M,
                                "c": ~arg[2] & M, "d": ~arg[3] & M, "N8": 8})
            if got != want:
                print(f"group {g + 1} leds={[hex(v) for v in leds]}:")
                for k in sorted(want):
                    print(f"  +{k}: want {want[k]:08x} got {got.get(k, 0):08x}")
                sys.exit(1)
            n += 1
    print(f"identical on {n} cases ({ninsns} vs {sum(map(len, groups))} instructions)")


if __name__ == "__main__":
    main()
