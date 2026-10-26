#!/usr/bin/env python3
"""Prove the ELF you are analysing is the image the chip is running.

BENCH.md section 6 makes this check mandatory, and makes the reason concrete: every
guard in this tree resolves names out of the ELF, so a symbol that exists proves the
ELF is new and says nothing at all about the chip. The check that does prove it is a
memory-image comparison against the flashed Intel HEX.

It is worth having as a file rather than hand-rolled, because the hand-rolled version
is easy to get subtly wrong. Intel HEX is ':' + count(2) + address(4) + type(2) +
data + checksum, and reading the type from the wrong two characters makes the high
half of the address the record type - so most records are silently treated as
something to skip. That parses a 30468-byte firmware as a couple of hundred bytes,
compares those, finds no disagreement, and reports success. It happened here twice.
So the byte accounting below is checked independently of the dictionary and a parser
that loses records fails loudly instead of passing on a stub.

A clean match still only proves ELF == hex file. To tie it to the chip, compare
against the file actually handed to `flash write_image`, or read the flash back.

  python3 tools/imgcmp.py firmware.hex firmware.elf      # ELF converted first
  python3 tools/imgcmp.py firmware.hex /tmp/from-chip.hex
"""

import argparse
import os
import subprocess
import sys
import tempfile


def parse_hex(path):
    """Intel HEX -> ({addr: "ab"}, declared_data_bytes). Raises on malformed input."""
    base = 0
    mem = {}
    declared = 0
    records = 0
    with open(path, "r", newline="") as fh:
        for lineno, line in enumerate(fh, 1):
            line = line.strip()
            if not line:
                continue
            if not line.startswith(":"):
                raise SystemExit(f"{path}:{lineno}: does not start with ':'")
            records += 1
            # : LL AAAA TT <data> CC
            count = int(line[1:3], 16)
            addr = int(line[3:7], 16)
            typ = int(line[7:9], 16)
            data = line[9:9 + 2 * count]
            if len(data) != 2 * count:
                raise SystemExit(f"{path}:{lineno}: record claims {count} bytes, "
                                 f"line has {len(data) // 2}")
            if typ == 0:
                declared += count
                for i in range(count):
                    mem[base + addr + i] = data[2 * i:2 * i + 2]
            elif typ == 4:          # extended linear address, sets bits 16-31
                base = int(data, 16) << 16
            elif typ == 2:          # extended segment address
                base = int(data, 16) << 4
            elif typ in (1, 5):     # end of file / start address
                pass
            else:
                raise SystemExit(f"{path}:{lineno}: unknown record type {typ:#04x}")
    # The point of this assertion: len(mem) can only be short of `declared` if a
    # record was skipped or two records were made to alias. A comparison that reads
    # almost nothing must not be allowed to look like a comparison that passed.
    if len(mem) != declared:
        raise SystemExit(
            f"{path}: {declared} data bytes declared but {len(mem)} distinct "
            f"addresses stored over {records} records - records are being skipped "
            f"or aliasing, do not trust any comparison of this file")
    return mem, declared


def to_hex(src, out):
    subprocess.run(["objcopy", "-O", "ihex", src, out], check=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("first", nargs="?", default="firmware.hex")
    ap.add_argument("second", nargs="?", default="firmware.elf")
    opt = ap.parse_args()
    a, b = opt.first, opt.second
    tmp = None
    if b.endswith(".elf"):
        tmp = tempfile.NamedTemporaryFile(suffix=".hex", delete=False).name
        to_hex(b, tmp)
        b_show, b_path = b, tmp
    else:
        b_show, b_path = b, b
    try:
        ma, na = parse_hex(a)
        mb, nb = parse_hex(b_path)
        print(f"{a}: {na} bytes")
        print(f"{b_show}: {nb} bytes")
        diff = [k for k in set(ma) & set(mb) if ma[k] != mb[k]]
        only_a = sorted(set(ma) - set(mb))
        only_b = sorted(set(mb) - set(ma))
        if not diff and not only_a and not only_b:
            print(f"IDENTICAL over {na} bytes")
            return 0
        print(f"MISMATCH: {len(diff)} bytes differ, {len(only_a)} present only in "
              f"{a}, {len(only_b)} only in {b_show}")
        for k in diff[:10]:
            print(f"  @{k:#010x}: {ma[k]} != {mb[k]}")
        for k, what in ((only_a, a), (only_b, b_show)):
            if k:
                print(f"  range only in {what}: {k[0]:#010x} .. {k[-1]:#010x}")
        return 1
    finally:
        if tmp:
            os.unlink(tmp)


if __name__ == "__main__":
    sys.exit(main())
