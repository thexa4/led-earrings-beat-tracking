#!/usr/bin/env python3
"""Check bitbang.h's inline assembly against the code it replaced.

The bitplane builder and the show loop in bitbang.h are inline assembly, so
the host tests never run them. This preprocesses bitbang.h and the versions
they replaced (tools/bitbang_ref.h) and pulls each function's instruction
string out, then:

- emulates the few Thumb instructions the builders use (lsls/lsrs/rors/adcs/
  eors/mvns/str/stmia) and compares the shown words for random colours, with the
  generic function's slot arguments permuted the way refreshLEDsRAM permutes
  them, and checks that all-off LEDs give the word clearLEDs fills in;
- runs both show functions with M0+ cycle counts (strh to the single-cycle
  GPIO port 1, ldr 2, other ALU 1, bne 2 taken and 1 not) and compares the
  pin writes: every group's bitplanes in the same order with the same on
  times, the same cycles between groups, the same twelve common pins, and
  each group's five bitplanes from one group's words.

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
    strings = r'((?:"(?:[^"\\]|\\.)*"\s*)+)'  # the template, up to the first ':'
    for body in re.findall(r"__asm__\s+__volatile__\s*\(\s*" + strings, m.group(1)):
        text = "".join(re.findall(r'"((?:[^"\\]|\\.)*)"', body))
        blocks.append([i.strip() for i in text.replace("\\t", "").split("\\n") if i.strip()])
    return blocks


def asm_of(name):
    return asm_blocks(name)[0]


def generic_prog():
    """bitbang_update_bitplane as straight-line code: the first bitplane, then
    the loop's four passes (each storing a word), without the loop counter."""
    (block,) = asm_blocks("bitbang_update_bitplane")
    loop = block.index("1:")
    first, plane = block[:loop], block[loop + 1:]
    body = re.search(r"\bbitbang_update_bitplane\s*\((.*?)\n}", src, re.S).group(1)
    if plane[-2:] != ["subs %[n], #1", "bne 1b"] or not re.search(r"\bn = 4;", body):
        sys.exit(f"bitbang_update_bitplane: want n = 4 and a loop ending subs #1, bne")
    plane = plane[:-2]
    return first + 4 * plane, len(block)


# Shown words: the per-group versions store three words, the generic one four
# and bitbang_show_leds skips the second (offsets in bytes, ref: generic).
SHOWN = {0: 0, 4: 8, 8: 12}
MASK = 0b1000011111111111  # the LED pins
OFF = MASK << 16 | MASK  # clearLEDs' word: every bitplane off


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
        if op == "stmia":  # stmia base!, {reg}: base counts bytes from 0
            base = re.fullmatch(r"%\[(\w+)\]!", args[0]).group(1)
            out[regs.get(base, 0)] = val(args[1].strip("{}"))
            regs[base] = regs.get(base, 0) + 4
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
    # refreshLEDsRAM: which LED goes to slots a, b, c, d for each group (None = off).
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
            if any(got[SHOWN[k]] != want[k] for k in want):
                print(f"group {g + 1} leds={[hex(v) for v in leds]}:")
                for k in sorted(want):
                    print(f"  +{k}: want {want[k]:08x} got {got[SHOWN[k]]:08x}")
                sys.exit(1)
            n += 1
    print(f"identical on {n} cases ({ninsns} vs {sum(map(len, groups))} instructions)")

    zeros = run(generic, {"tmp": 0, "a": M, "b": M, "c": M, "d": M, "N8": 8})
    if any(zeros[k] != OFF for k in SHOWN.values()):
        sys.exit(f"all off: {[hex(zeros[k]) for k in sorted(zeros)]}, want {OFF:#x}")
    print(f"all off gives {OFF:#010x}, the word clearLEDs fills in")

    ref = show_trace("bitbang_show_leds_twelve", 12)
    new = show_trace("bitbang_show_leds", 16)
    if ref != new:
        for name, t in (("twelve copies", ref), ("loop", new)):
            print(f"{name}:")
            for row in t:
                print("  ", row)
        sys.exit("show timing differs")
    groups, pins = new
    g = groups[0]  # (dt, ODR, bitplane) x 5, (dt, ODR, off), (dt, OTYPER)
    for (dt, _, plane), nxt in zip(g[:5], g[1:6]):
        if nxt[0] != plane:
            sys.exit(f"bitplane {plane} is on for {nxt[0]} cycles")
    if len(groups) != 12 or len(pins) != 12 or g[6][0] != 1:
        sys.exit(f"{len(groups)} groups, {len(pins)} pins, {g[6][0]} cycles between groups")
    print(f"show timing identical: 12 groups, bitplanes on for "
          f"{'/'.join(str(p) for _, _, p in g[:5])} cycles, 1 cycle between groups")


def show_trace(name, stride):
    """The pin writes of show function name over twelve groups of words
    stride bytes apart: per group, the writes from its OTYPER store up to the
    next one (the last group: up to OTYPER's restore) as (cycles since the
    previous write, register, bitplane or 'off'), each group checked to show
    one group's words; and the set of common pins. Writes before the first
    OTYPER store don't count (the loop starts by ending the previous group's
    16 cycle bitplane, which on the first pass are pins already off)."""
    prog = [i for block in asm_blocks(name) for i in block]
    # Each half word tells where it came from: group, word, half.
    mem = {}
    for g in range(13):  # the loop reads one word past the last group
        for w in range(stride // 4):
            mem[0x1000 + g * stride + 4 * w] = (g << 4 | w << 1 | 1) << 16 | g << 4 | w << 1
    layout = ({0: 2, 1: 1, 2: 8, 3: 4, 4: 16} if stride == 12 else
              {0: 2, 1: 1, 4: 8, 5: 4, 6: 16})  # word << 1 | half: bitplane
    writes = run_show(prog, {"bitplanes": 0x1000, "gpio": 0x5000_0000, "mask": MASK}, mem)
    groups, pins, cur, used, last = [], set(), None, None, 0
    for t, reg, v in writes:
        if reg == "OTYPER" and cur is None and not pins:
            last = t
        if reg == "OTYPER":
            if cur is not None:
                groups.append(tuple(cur + [(t - last, "end" if v == MASK else reg)]))
            if v == MASK:  # the restore at the end
                cur = None
                continue
            pin = MASK & ~v
            if pin & (pin - 1) or pin in pins:
                sys.exit(f"{name}: OTYPER {v:#x} is not a new single pin")
            pins.add(pin)
            cur, used = [], None
        elif cur is not None:
            if v == MASK:
                label = "off"
            else:
                if used not in (None, v >> 4):
                    sys.exit(f"{name}: one group shows words of groups {used} and {v >> 4}")
                used = v >> 4
                label = layout[v & 15]
            cur.append((t - last, reg, label))
        last = t
    # From the first OTYPER store to the first bitplane all pins are off: the
    # twelve copies load in between, the loop already has its word.
    groups[0] = ((None,) + groups[0][0][1:],) + groups[0][1:]
    return tuple(groups), frozenset(pins)


def run_show(prog, regs, mem):
    """Runs a show function's instructions with M0+ cycle counts; returns its
    GPIO stores as (cycle, register, value)."""
    labels = {ins[:-1]: i for i, ins in enumerate(prog) if ins.endswith(":")}
    regs = dict(regs)
    gpio = {0x4: "OTYPER", 0x14: "ODR"}
    t, pc, z, out = 0, 0, False, []

    def val(op):
        op = op.strip()
        if op.startswith("#"):
            return int(op[1:], 0)
        return regs[re.fullmatch(r"%\[(\w+)\]", op).group(1)]

    def addr(op):
        m = re.fullmatch(r"\[%\[(\w+)\],\s*#(\w+)\]", op)
        return regs[m.group(1)] + int(m.group(2), 0)

    while pc < len(prog):
        ins = prog[pc]
        pc += 1
        if ins.endswith(":"):
            continue
        op, _, rest = ins.partition(" ")
        args = [a.strip() for a in re.split(r",(?![^\[]*\])", rest)] if rest else []
        if op == "nop":
            t += 1
            continue
        if op == "bne":
            if not z:
                pc = labels[args[0].rstrip("b")]
                t += 2
            else:
                t += 1
            continue
        if op == "strh":
            out.append((t, gpio[addr(args[1]) - regs["gpio"]], val(args[0]) & 0xFFFF))
            t += 1
            continue
        d = re.fullmatch(r"%\[(\w+)\]", args[0]).group(1)
        src = args[1:] if len(args) > 1 else []
        if op == "ldr":
            x = mem.get(addr(src[0]), 0)
            t += 2
        else:
            # Two-operand forms (lsls rd, #n) act on rd; three-operand ones
            # (lsls rd, rn, #n) on rn.
            if len(src) == 2:
                a, b = val(src[0]), val(src[1])
            else:
                a, b = regs.get(d, 0), val(src[0])
            if op == "movs":
                x = b
            elif op == "negs":
                x = -b & M
            elif op == "ands":
                x = a & b
            elif op == "eors":
                x = a ^ b
            elif op == "adds":
                x = (a + b) & M
            elif op == "lsls":
                x = (a << b) & M
            elif op == "lsrs":
                x = a >> b
            elif op == "cmp":
                z = a == b
                t += 1
                continue
            else:
                sys.exit(f"unhandled instruction {ins!r}")
            z = x == 0
            t += 1
        regs[d] = x
    return out


if __name__ == "__main__":
    main()
