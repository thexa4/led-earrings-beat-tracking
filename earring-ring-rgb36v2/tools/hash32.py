#!/usr/bin/env python3
"""hash32 (hardware.go) of a file: what tools/screamtelem.py shows as the
last message's hash once an earring has received it.

  tools/hash32.py pattern.bin
"""
import struct
import sys

M = 0xFFFFFFFF


def hash32(b):
    # ecc.MurmurHash32 over the whole words, then FNV-style over the tail.
    n = len(b) // 4
    h = 0
    for (k,) in struct.iter_unpack("<I", b[: n * 4]):
        k = k * 0xCC9E2D51 & M
        k = ((k << 15) | (k >> 17)) & M
        k = k * 0x1B873593 & M
        h ^= k
        h = ((h << 13) | (h >> 19)) & M
        h = (h * 5 + 0xE6546B64) & M
    h ^= n * 4
    h ^= h >> 16
    h = h * 0x85EBCA6B & M
    h ^= h >> 13
    h = h * 0xC2B2AE35 & M
    h ^= h >> 16
    for c in b[n * 4:]:
        h ^= c
        h = h * 16777619 & M
    return h ^ len(b)


if __name__ == "__main__":
    b = open(sys.argv[1], "rb").read()
    print("%d bytes, hash32 %08x" % (len(b), hash32(b)))
