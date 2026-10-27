#!/usr/bin/env python3
"""Reference decoder for "scream" audio: the sound produced by the 📢 button at
https://aykevl.nl/apps/led-editor/

It is an independent implementation of the transport described in
../ENCODING.md, written to verify that description. It is deliberately simple
and slow (numpy per symbol) and does not model the receiver's timing recovery.

Usage:
    ./decode.py out.wav [-o out.bin] [--expect animation.bin] [--packets]

Requires numpy.
"""

import argparse
import struct
import sys

import numpy as np

# --- protocol constants (see ENCODING.md) -----------------------------------
SYMBOLS_PER_SECOND = 50
FREQ_START = 400      # Hz
FREQ_STEP = 50        # Hz == SYMBOLS_PER_SECOND
NUM_FREQS = 32

MAGIC1 = 0b10_10_01_01_01_10_10_01_10_01_10_10_01_10_10_11
MAGIC1_RECV = 0b1100_0110_1011_0111
MAGIC2 = 0x8FDC4E43
MAGIC3 = 0x7C39D645
WHITENING_MAGIC = 0xF1F96758
PREAMBLE_SYMBOLS = 8  # 3x sync, 1x inverted sync, magic2, ~magic2, magic3, ~magic3

MASK = 0xFFFFFFFF


def xorshift32(state):
    state ^= (state << 13) & MASK
    state ^= state >> 17
    state ^= (state << 5) & MASK
    return state & MASK


def murmur32(words, seed):
    """murmur3 x86_32 over a buffer of 32-bit words (no tail handling)."""
    h = seed
    for k in words:
        k = (k * 0xCC9E2D51) & MASK
        k = ((k << 15) | (k >> 17)) & MASK
        k = (k * 0x1B873593) & MASK
        h ^= k
        h = ((h << 13) | (h >> 19)) & MASK
        h = (h * 5 + 0xE6546B64) & MASK
    h ^= (len(words) * 4) & MASK
    h ^= h >> 16
    h = (h * 0x85EBCA6B) & MASK
    h ^= h >> 13
    h = (h * 0xC2B2AE35) & MASK
    h ^= h >> 16
    return h


def next_packet(sequence, num_symbols):
    """The set of source symbols XORed into the packet with this sequence no."""
    rand = (sequence * 0x970DA0E2) & MASK
    # Hand-tuned tables for very small messages (zero overhead when lossless).
    if num_symbols == 2:
        return (0b10_01_11 >> ((sequence % 3) * 2)) & 0b11
    if num_symbols == 3:
        return (0b001_010_100_111 >> ((sequence % 4) * 3)) & 0b111
    if num_symbols == 4:
        return (0b0001_1001_1100_0010_0011_0100_1010_1111 >> ((sequence % 8) * 4)) & 0b1111

    chosen = rand >> 24
    if num_symbols <= 14:
        d1, d3, d4, d5, d12 = 77, 87, 46, 33, 13
        if chosen < d1:
            deg = 1
        elif chosen < d1 + d3:
            deg = 3
        elif chosen < d1 + d3 + d4:
            deg = 4
        elif chosen < d1 + d3 + d4 + d5:
            deg = 5
        else:
            deg = 12
    else:
        d2, d3, d1, d12, d11 = 81, 81, 39, 34, 21
        if chosen < d2:
            deg = 2
        elif chosen < d2 + d3:
            deg = 3
        elif chosen < d2 + d3 + d1:
            deg = 1
        elif chosen < d2 + d3 + d1 + d12:
            deg = 12
        else:
            deg = 11
    deg = min(deg, num_symbols)

    if deg == 1:
        # Degree-1 packets are handed out in order, not at random.
        return 1 << (sequence % num_symbols)

    packet_set, selected = 0, 0
    while selected < deg:
        rand = xorshift32(rand)
        pick = rand % 32
        if pick >= num_symbols or packet_set & (1 << pick):
            continue
        selected += 1
        packet_set |= 1 << pick
    return packet_set


# ---------------------------------------------------------------- input ----

def read_wav(path):
    raw = open(path, "rb").read()
    assert raw[:4] == b"RIFF" and raw[8:12] == b"WAVE", "not a RIFF/WAVE file"
    i, fmt, data = 12, None, None
    while i + 8 <= len(raw):
        cid = raw[i:i + 4]
        size = struct.unpack_from("<I", raw, i + 4)[0]
        if cid == b"fmt ":
            fmt = struct.unpack_from("<HHIIHH", raw, i + 8)
        elif cid == b"data":
            data = raw[i + 8:i + 8 + size]
        i += 8 + size + (size & 1)
    channels, rate, bits = fmt[1], fmt[2], fmt[5]
    assert bits == 16, f"only 16-bit PCM supported, got {bits}"
    x = np.frombuffer(data, dtype="<i2").astype(np.float64) / 32768.0
    if channels > 1:
        x = x.reshape(-1, channels).mean(axis=1)
    return x, rate


def symbol_magnitudes(x, rate):
    """Magnitude of each of the 32 channels, for every symbol window.

    Each channel frequency is a whole number of cycles per window, so a plain
    DFT bin is equivalent to the Goertzel result the device uses (which also
    yields a phase, used only for timing here).
    """
    n = rate // SYMBOLS_PER_SECOND
    assert n * SYMBOLS_PER_SECOND == rate, "sample rate must be a multiple of 50"
    cycles = (FREQ_START + FREQ_STEP * np.arange(NUM_FREQS)) * n / rate
    assert np.allclose(cycles, np.round(cycles)), "channels not orthogonal"
    t = np.arange(n)
    kernel = np.exp(-2j * np.pi * np.outer(cycles, t) / n)
    count = len(x) // n
    out = np.empty((count, NUM_FREQS))
    for s in range(count):
        out[s] = 2 * np.abs(kernel @ x[s * n:(s + 1) * n]) / n
    return out, n


# --------------------------------------------------------------- decoding ---

def fsk_pattern(mags, sym):
    """16-bit 'which tone of each pair is louder' pattern of a sync symbol."""
    bits = 0
    for i in range(16):
        bits >>= 1
        if mags[sym][2 * i] < mags[sym][2 * i + 1]:
            bits |= 1 << 15
    return bits


def calibrate(mags):
    """Per-channel on/off reference levels and threshold, from the magic."""
    on = np.zeros(NUM_FREQS)
    off = np.zeros(NUM_FREQS)
    for sym, magic in ((4, MAGIC2), (5, ~MAGIC2 & MASK),
                       (6, MAGIC3), (7, ~MAGIC3 & MASK)):
        for i in range(NUM_FREQS):
            target = on if (magic >> i) & 1 else off
            target[i] += mags[sym][i] / 2
    return on, off, (on + off) / 2


def solve(rows, num_symbols, words_per_packet, num_words):
    """GF(2) elimination, one system per word position.

    Solving per global word (rather than per symbol vector) keeps the short
    final source symbol correct: the encoder contributes nothing for the
    padding words, so they must not be treated as unknowns.
    """
    solved = {}
    for w in range(num_words):
        offset = w % words_per_packet
        pivot = {}
        for mask, packet in rows:
            m, v = mask, packet[offset]
            while m:
                i = (m & -m).bit_length() - 1
                if i in pivot:
                    pm, pv = pivot[i]
                    m ^= pm
                    v ^= pv
                else:
                    pivot[i] = (m, v)
                    break
        known = {}
        for i in sorted(pivot, reverse=True):
            m, v = pivot[i]
            for j in range(i + 1, num_symbols):
                if (m >> j) & 1 and j in known:
                    v ^= known[j]
                    m ^= 1 << j
            if m == 1 << i:
                known[i] = v
        owner = w // words_per_packet
        if owner in known:
            solved[w] = known[owner]
    return solved


def decode(mags, show_packets=False):
    on, off, thr = calibrate(mags)
    print(f"mean on/off level: {on.mean():.4f} / {off.mean():.4f}  "
          f"(level separation {20*np.log10(on.mean()/max(off.mean(), 1e-12)):.1f} dB)")
    for sym, magic in ((4, MAGIC2), (5, ~MAGIC2 & MASK),
                       (6, MAGIC3), (7, ~MAGIC3 & MASK)):
        got = sum(1 << i for i in range(NUM_FREQS) if mags[sym][i] > thr[i])
        errors = bin(got ^ magic).count("1")
        print(f"  magic symbol {sym}: {'OK' if errors == 0 else f'{errors} bad bits'}")
    for sym in range(4):
        pat = fsk_pattern(mags, sym)
        expect = MAGIC1_RECV if sym < 3 else ~MAGIC1_RECV & 0xFFFF
        # The lowest pair is '11' in the sync symbol, so that bit is a coin flip.
        errors = bin((pat ^ expect) & ~1).count("1")
        print(f"  sync symbol {sym}: {'OK' if errors == 0 else f'{errors} bad bits'}")

    words = [sum(1 << i for i in range(NUM_FREQS) if mags[sym][i] > thr[i])
             for sym in range(len(mags))]

    pos = 0
    rows = []
    done_at = None
    info = {}
    rounds = 0
    good = bad = 0
    while pos + PREAMBLE_SYMBOLS + 5 <= len(words):
        pos += PREAMBLE_SYMBOLS
        rounds += 1
        whitening = WHITENING_MAGIC
        header = []
        for _ in range(3):
            header.append(words[pos] ^ whitening)
            whitening = xorshift32(whitening)
            pos += 1
        header_hash, _parity = words[pos], words[pos + 1]
        pos += 2
        if (murmur32(header, 0) | 0b11) != header_hash:
            print("header hash mismatch, stopping")
            break
        w0, w1, w2 = header
        length = (w0 >> 16) & 0xFFFF
        wpp = (w1 & 0xFF) + 1
        total = ((w1 >> 8) & 0xFF) + 1
        seq = (w1 >> 16) & 0xFFFF
        message_hash = w2
        num_symbols = (length + 4 * wpp - 1) // (4 * wpp)
        num_words = (length + 3) // 4
        t = (pos - 5) / SYMBOLS_PER_SECOND
        print(f"[{t:6.2f}s] round {rounds}: header len={length} words/pkt={wpp} "
              f"packets={total} seq={seq} hash={message_hash:#010x} "
              f"symbols={num_symbols}")
        info.update(length=length, words_per_packet=wpp, num_symbols=num_symbols,
                    num_words=num_words, packets_per_round=total,
                    message_hash=message_hash)

        for pkt in range(total):
            if pos + wpp + 2 > len(words):
                break
            packet = []
            for _ in range(wpp):
                packet.append(words[pos] ^ whitening)
                whitening = xorshift32(whitening)
                pos += 1
            packet_hash = words[pos]
            pos += 2  # hash symbol + parity symbol
            if (murmur32(packet, message_hash ^ seq) | 0b11) != packet_hash:
                bad += 1
                continue
            good += 1
            mask = next_packet(seq, num_symbols)
            rows.append((mask, packet))
            if show_packets:
                print(f"  [{pos / SYMBOLS_PER_SECOND:6.2f}s] packet seq={seq} "
                      f"degree={bin(mask).count('1')} mask={mask:08b}")
            if done_at is None and len(rows) >= num_symbols:
                solved = solve(rows, num_symbols, wpp, num_words)
                if len(solved) == num_words:
                    done_at = pos / SYMBOLS_PER_SECOND
                    data = b"".join(solved[w].to_bytes(4, "little")
                                    for w in range(num_words))
                    info["data"] = data[:length]
                    padded = data[:length] + b"\0" * ((-length) % 4)
                    got = murmur32([int.from_bytes(padded[i:i + 4], "little")
                                    for i in range(0, len(padded), 4)], 0)
                    print(f"[{done_at:6.2f}s] complete: {len(rows)} packets "
                          f"({good} good / {bad} bad), "
                          f"message hash {'OK' if got == message_hash else 'MISMATCH'}")
            seq = seq + 1 if seq + 1 != 0 else 1
    info["rounds"] = rounds
    return info


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("wav")
    ap.add_argument("-o", "--output")
    ap.add_argument("--expect", help="fail if the payload differs from this file")
    ap.add_argument("--packets", action="store_true", help="list every packet")
    args = ap.parse_args()

    x, rate = read_wav(args.wav)
    print(f"{rate} Hz, {rate // SYMBOLS_PER_SECOND} samples/symbol, "
          f"{len(x)/rate:.2f} s, peak {np.abs(x).max():.3f}, "
          f"rms {np.sqrt((x * x).mean()):.4f}")
    mags, _ = symbol_magnitudes(x, rate)
    info = decode(mags, args.packets)

    data = info.get("data")
    if data is None:
        print("NOT DECODED", file=sys.stderr)
        sys.exit(1)
    print(f"decoded {len(data)} bytes")
    if args.output:
        open(args.output, "wb").write(data)
    if args.expect:
        want = open(args.expect, "rb").read()
        if want != data:
            print(f"DIFFERS from {args.expect}", file=sys.stderr)
            sys.exit(2)
        print(f"identical to {args.expect}")


main()
