#!/usr/bin/env python3
"""Read the pattern decision state from both earrings and compare the trajectories.

The paired scorers report whether the two earrings show the same colour; this
reports *why*, by reading the three numbers the choice is made of - the spectral
centroid, its own running reference, and its mean absolute deviation - from each
device's RAM at the same moment.

That distinction matters because the two fail differently. If the centroids track
each other but sit at different levels, the selector is fine and the reference is
not doing its job. If cent - centAvg holds one sign for many seconds on each
device, the ratio is saturating and the pattern is a sign detector, which pins one
earring at its bottom colour and the other at its top and is invisible to an
agreement number alone.

Offsets come from the firmware ELF's own debug info, never hardcoded: a field read
at a stale offset still returns a plausible-looking number.

  tools/patternstate.py [--seconds 30] [--interval 0.5]
"""
import argparse
import math
import os
import struct
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem

PORTS = {"L": 4445, "R": 4446}


def member_offsets(elf, names):
    """Byte offsets of named Pipeline members, from the ELF's DWARF.

    Parsed per DIE rather than line by line: the attributes of a DW_TAG_member
    arrive in an order that depends on its abbreviation, so pairing a
    data_member_location with the most recently seen name silently attributes it to
    the previous member - which yields plausible-looking numbers read from the
    wrong field, the exact failure this function exists to avoid.

    Where a name appears in more than one struct, the highest offset wins: the
    Pipeline is the largest type involved and sits last.
    """
    import subprocess
    txt = subprocess.run(["readelf", "--debug-dump=info", elf],
                         capture_output=True, text=True).stdout.splitlines()
    out = {}
    die_name, die_off, is_member = None, None, False

    def flush():
        nonlocal die_name, die_off, is_member
        if is_member and die_name in names and die_off is not None:
            if die_off >= out.get(die_name, -1):
                out[die_name] = die_off
        die_name, die_off, is_member = None, None, False

    for line in txt:
        stripped = line.strip()
        # A new DIE at member depth starts with "<depth><offset>: Abbrev Number".
        if stripped.startswith("<") and "Abbrev Number" in stripped:
            flush()
            is_member = "DW_TAG_member" in stripped
            continue
        if "DW_AT_name" in stripped:
            die_name = stripped.rsplit(":", 1)[-1].strip()
        elif "DW_AT_data_member_location:" in stripped:
            try:
                die_off = int(stripped.split(":")[-1])
            except ValueError:
                die_off = None
    flush()
    return out


def s32(u):
    return struct.unpack("<i", struct.pack("<I", u))[0]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--elf", default=os.path.join(os.path.dirname(
        os.path.abspath(__file__)), "..", "firmware.elf"))
    ap.add_argument("--seconds", type=float, default=30)
    ap.add_argument("--interval", type=float, default=0.5)
    ap.add_argument("--outdir", default=os.path.expanduser("~/psync/pstate"))
    ap.add_argument("--dump", default="",
                    help="write the sampled trajectory here as CSV (t,side,z,gap,dev,pat,bands...)")
    args = ap.parse_args()

    names = {"cent", "centAvg", "centDev", "smooth", "smoothSnap", "agcShift",
             "total", "frame", "lastPattern"}
    off = member_offsets(args.elf, names)
    for n in sorted(names):
        if n not in off:
            sys.exit(f"DWARF has no Pipeline member {n!r} - stale elf?")
    print("offsets: " + " ".join(f"{n}={off[n]}" for n in sorted(names)))

    os.makedirs(args.outdir, exist_ok=True)
    ocds = {}
    for name, port in PORTS.items():
        ocds[name] = telem.OpenOCD("localhost", port)
        ocds[name].cmd("version")

    # syncPipe is the Pipeline's address; resolve from symbol table.
    import subprocess
    syms = subprocess.run(["readelf", "-sW", args.elf], capture_output=True,
                          text=True).stdout
    addr = {}
    for line in syms.splitlines():
        for sym in ("main.syncPipe", "syncdsp.Telem"):
            if line.strip().endswith(sym):
                addr[sym] = int(line.split()[1], 16)
    pipe = addr["main.syncPipe"]
    print(f"syncPipe @0x{pipe:08x}")

    rows = {k: [] for k in PORTS}
    t0 = time.time()
    while time.time() - t0 < args.seconds:
        now = time.time() - t0
        need = max(off.values()) + 8
        for name, oc in ocds.items():
            w = oc.mdw(pipe, need // 4 + 1)

            def g(n, signed=True):
                v = w[off[n] // 4]
                return s32(v) if signed else v

            bands = [s32(w[(off["smooth"] // 4) + i]) for i in range(8)]
            rec = dict(t=now, cent=g("cent"), centAvg=g("centAvg"),
                       centDev=g("centDev"), frame=g("frame"),
                       pat=g("lastPattern"), agc=g("agcShift"),
                       bands=bands)
            # What PatternIndex will say, recomputed here from the same three
            # numbers so the firmware's arithmetic is checked rather than assumed.
            d = rec["cent"] - rec["centAvg"]
            dev = max(rec["centDev"], 1)
            z = max(-2 * 256, min(2 * 256, (d * 256) // dev))
            rec["gap"], rec["z"] = d, z / 256
            rows[name].append(rec)
        time.sleep(args.interval)

    if args.dump:
        import csv
        with open(args.dump, "w", newline="") as fh:
            w = csv.writer(fh)
            w.writerow(["t", "side", "z", "gap", "dev", "pat", "frame", "agc"]
                       + [f"b{i}" for i in range(8)])
            for name, rr in rows.items():
                for r in rr:
                    w.writerow([f"{r['t']:.3f}", name, f"{r['z']:.4f}", r["gap"],
                                r["centDev"], r["pat"], r["frame"], r["agc"]]
                               + r["bands"])
        print(f"wrote {args.dump}")

    for name, rr in rows.items():
        if len(rr) < 5:
            print(f"{name}: only {len(rr)} samples - device not running?")
            continue
        zs = [r["z"] for r in rr]
        pos = sum(1 for z in zs if z > 0) / len(zs)
        sat = sum(1 for z in zs if abs(z) >= 1.99) / len(zs)
        over = sum(1 for z in zs if z > 0.844) / len(zs)
        under = sum(1 for z in zs if z < -0.844) / len(zs)
        devs = [r["centDev"] for r in rr]
        pats = [r["pat"] for r in rr]
        agcs = sorted(set(r["agc"] for r in rr))
        print(f"\n{name}: n={len(rr)}  centDev median={sorted(devs)[len(devs)//2]:.0f}"
              f"  min={min(devs):.0f} max={max(devs):.0f}  agcShift seen={agcs}")
        print(f"   cent-centAvg > 0 in {100*pos:.0f}% of samples"
              f"   |z|>0.844 in {100*(over+under):.0f}%"
              f"   saturated |z|~2 in {100*sat:.0f}%")
        hist = [pats.count(p) for p in range(4)]
        print("   pattern occupancy " + " ".join(
            f"{p}:{100*h/len(pats):.0f}%" for p, h in enumerate(hist)))
        print("   z series (last 24): " +
              " ".join(f"{z:+.2f}" for z in zs[-24:]))

    # Longest run of one sign for gap: the direct test of "pinned at an end".
    for name, rr in rows.items():
        best = cur = 0
        prev = 0
        for r in rr:
            s = 1 if r["gap"] > 0 else -1
            cur = cur + 1 if s == prev else 1
            prev = s
            best = max(best, cur)
        secs = best * args.interval
        print(f"{name}: longest same-sign stretch of cent-centAvg = "
              f"{best} samples (~{secs:.1f} s)")

    # The spectra behind those numbers. centDev is computed in the log domain,
    # where a gain is a constant that cent, centAvg and centDev all move by - so
    # two devices hearing the same music cannot differ in centDev because one is
    # quieter. A factor between them therefore means the two are hearing different
    # SPECTRA, which is the thing no self-referenced threshold can repair. Printing
    # the mean band level with each device's own mean removed says directly how far
    # apart the shapes are, band by band, in Q8 log2 (256 = one octave).
    print("\nmean smoothed spectrum, each with its own overall level removed:")
    prof = {}
    for name, rr in rows.items():
        if not rr:
            continue
        m = [sum(r["bands"][i] for r in rr) / len(rr) for i in range(8)]
        avg = sum(m) / len(m)
        prof[name] = [v - avg for v in m]
    print("  band:      " + " ".join(f"{i:>7d}" for i in range(8)))
    for name, p in prof.items():
        print(f"  {name} shape: " + " ".join(f"{v:+7.0f}" for v in p))
    if len(prof) == 2:
        (na, pa), (nb, pb) = prof.items()
        d = [a - b for a, b in zip(pa, pb)]
        print(f"  {na}-{nb}:    " + " ".join(f"{v:+7.0f}" for v in d))
        print(f"  mean |shape difference| = {sum(abs(v) for v in d)/8:.0f} Q8"
              f"  ({sum(abs(v) for v in d)/8/256:.2f} octaves)")
        # The ramp is the pattern weights; their difference is what the centroid
        # sees, in the same units cent is in.
        ramp = [-7, -5, -3, -1, 1, 3, 5, 7]
        print(f"  weighted (ramp) shape sum: " +
              "  ".join(f"{n}={sum(w*v for w, v in zip(ramp, p)):+.0f}"
                        for n, p in prof.items()))

    # The mean shapes agreeing is not the same as the two devices SEEING the same
    # thing. The pattern is driven by how far the centroid swings from its own
    # reference, so what matters is each band's dynamic range over the window: a
    # device whose spectrum barely moves has a small spread, and dividing by that
    # spread turns its leftover drift and noise into a confident-looking opinion.
    # Printed as the peak-to-peak excursion of each band, in Q8 log2 (256 = octave).
    print("\nper-band peak-to-peak excursion over the window (Q8 log2):")
    print("  band:      " + " ".join(f"{i:>7d}" for i in range(8)))
    for name, rr in rows.items():
        if not rr:
            continue
        rng = [max(r["bands"][i] for r in rr) - min(r["bands"][i] for r in rr)
               for i in range(8)]
        print(f"  {name} swing: " + " ".join(f"{v:+7d}" for v in rng))

    # Do the two devices' decisions move together at all? Correlating the scaled
    # deviations says whether they disagree about the sign of the moment (which no
    # scaling can fix) or only about its size (which a shared scale could).
    if all(rows.get(k) for k in PORTS) and len(rows["L"]) == len(rows["R"]):
        zl = [r["z"] for r in rows["L"]]
        zr = [r["z"] for r in rows["R"]]
        n = len(zl)
        ml, mr = sum(zl) / n, sum(zr) / n
        cov = sum((a - ml) * (b - mr) for a, b in zip(zl, zr)) / n
        vl = sum((a - ml) ** 2 for a in zl) / n
        vr = sum((b - mr) ** 2 for b in zr) / n
        r = cov / math.sqrt(vl * vr) if vl > 0 and vr > 0 else float("nan")
        same = sum(1 for a, b in zip(zl, zr) if (a > 0) == (b > 0)) / n
        print(f"\nz-series correlation r={r:+.3f}; same sign in {100*same:.0f}% of samples")
        print(f"  mean z: L={ml:+.2f} R={mr:+.2f}  (offset {ml-mr:+.2f} MAD)"
              f"  spread: L={math.sqrt(vl):.2f} R={math.sqrt(vr):.2f}")
        print("  if the signs agree and only the size differs, the cuts - not the "
              "feature - are what separate the two earrings.")

    for oc in ocds.values():
        oc.close()


if __name__ == "__main__":
    main()
