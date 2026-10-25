#!/usr/bin/env python3
"""Poll the earring's SWD telemetry ring over an OpenOCD telnet port.

The debug-branch firmware (syncdsp/telemetry.go) records one 4-word record per
analysis frame into syncdsp.Telem while the pipeline runs; reading RAM over
OpenOCD's telnet port does not halt the CPU, so this is a non-intrusive tap.
Tunables are const on this branch (see consts.go), so parameter sweeps go
through tools/exp.sh (edit + rebuild + flash) rather than live pokes.

Usage examples:
  tools/telem.py --seconds 20 --play 120:20 --out run.csv --plot run.png
  tools/telem.py --peek syncdsp.Telem        # sanity: dump control block
  tools/telem.py --seconds 60 --play 300:60 --quiet

Requires: openocd reachable adapter, firmware.elf next to the script's parent
(for symbol addresses), ffplay for --play. Audio only works via ffplay on
this machine (pw-play/paplay route but stay silent).
"""

import argparse
import csv
import re
import socket
import subprocess
import sys
import time

PODCAST_DEFAULT = "/home/max/Documents/led-earrings/podcast.mp3"

# Control block word layout (must match syncdsp.Telem in telemetry.go).
CTRL_MAGIC, CTRL_SEQ, CTRL_HEAD = 0, 1, 2
CTRL_HOSTMARK, CTRL_LASTMARK = 3, 4
CTRL_LASTANCHOR, CTRL_LASTPATTERN, CTRL_LASTLOCK, CTRL_LASTUNLOCK = 5, 6, 7, 8
CTRL_WORDS, TELEMETRIES_OFFSET = 10, 9  # ring starts at word 9

# Ring depth: must match TelemRecords in syncdsp/telemetry.go.
RECORDS = 40

# Record layout (4 words), see telemetry.go for the prose version.
FLAG = dict(onset=1 << 0, beat=1 << 1, bar=1 << 2, anchor=1 << 3,
            pchange=1 << 4, locked=1 << 5, silent=1 << 6, pushed=1 << 7,
            mark=1 << 11, returned=1 << 12)
NA = 0xFF  # distance field not computed / no ring entry


class OpenOCD:
    """Minimal telnet client for the OpenOCD command port.

    OpenOCD echoes each command *after* the previous reply, so reading "until
    the prompt" returns the previous command's output. Sync instead by sending
    a `puts <marker>` after every command and reading until the marker shows
    up: what precedes it is exactly this command's output.
    """

    def __init__(self, host="localhost", port=4444):
        self.sock = socket.create_connection((host, port), timeout=5)
        self.buf = b""
        self.seq = 0
        self._read_until(b"> ")  # banner

    def _read_until(self, marker, timeout=5.0):
        end = time.time() + timeout
        while marker not in self.buf:
            if time.time() > end:
                raise TimeoutError(f"openocd reply {marker!r} timed out")
            data = self.sock.recv(4096)
            if not data:
                raise ConnectionError("openocd closed the connection")
            self.buf += data
        out, self.buf = self.buf.split(marker, 1)
        return re.sub(rb"[^\x20-\x7e\n]", b"", out).decode()

    def cmd(self, line):
        self.seq += 1
        marker = f"###DONE{self.seq}"
        self.sock.sendall((line + "\n").encode())
        self.sock.sendall(f"puts {marker}\n".encode())
        return self._read_until(marker.encode())

    def mdw(self, addr, count):
        """Read `count` 32-bit words at `addr`. Returns a list of ints."""
        out = []
        step = 64  # OpenOCD prints 8/line; chunk to bound one response
        while count > 0:
            n = min(count, step)
            text = self.cmd(f"mdw 0x{addr:08x} {n}")
            words = re.findall(r"(?<![0-9a-fA-Fx])[0-9a-fA-F]{8}(?![0-9a-fA-F])", text)
            if len(words) != n:
                raise RuntimeError(f"mdw reply has {len(words)}/{n} words: {text!r}")
            out += [int(w, 16) for w in words]
            addr += 4 * n
            count -= n
        return out

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


def server_up(host, port):
    try:
        socket.create_connection((host, port), timeout=0.5).close()
        return True
    except OSError:
        return False


def spawn_server(cwd):
    """Start a persistent OpenOCD server with telnet, if none is running."""
    if server_up("localhost", 4444):
        return
    subprocess.Popen(
        ["openocd", "-f", "interface/stlink.cfg", "-f", "target/stm32l0.cfg",
         "-c", "telnet_port 4444", "-c", "init", "-c", "reset run"],
        cwd=cwd, stdout=open("/tmp/oocd.log", "wb"), stderr=subprocess.STDOUT)
    for _ in range(20):
        if server_up("localhost", 4444):
            return
        time.sleep(0.25)
    raise RuntimeError("openocd telnet port did not come up; see /tmp/oocd.log")


def resolve_symbols(elf):
    """All ELF symbols as {name: address}; the telemetry address is the one
    named syncdsp.Telem (the Go package path is the full symbol)."""
    text = subprocess.run(["readelf", "-sW", elf], capture_output=True,
                          text=True, check=True).stdout
    syms = {}
    for line in text.splitlines():
        m = re.match(r"\s*\d+:\s+([0-9a-f]+)\s+\S+\s+\S+\s+\S+\s+\S+\s+\S+\s+(.+)$", line)
        if m:
            syms[m.group(2)] = int(m.group(1), 16)
    return syms


def decode(seq, words):
    """One 4-word record -> flat dict (fields mirror telemetry.go)."""
    hash_, flags, w2, w3 = words
    return dict(
        seq=seq,
        hash=f"{hash_:08x}",
        onset=bool(flags & FLAG["onset"]), beat=bool(flags & FLAG["beat"]),
        bar=bool(flags & FLAG["bar"]), anchor=bool(flags & FLAG["anchor"]),
        pchange=bool(flags & FLAG["pchange"]), locked=bool(flags & FLAG["locked"]),
        silent=bool(flags & FLAG["silent"]), pushed=bool(flags & FLAG["pushed"]),
        mark=bool(flags & FLAG["mark"]), returned=bool(flags & FLAG["returned"]),
        pattern=(flags >> 8) & 7, votes=(flags >> 16) & 0xFF,
        period=(flags >> 24) & 0x7F,
        total=w2 & 0xFFFF, phase=(w2 >> 16) & 0x7F,
        onset_age=(w2 >> 23) & 0x7F,
        settled=bool(w2 >> 30), primed=bool(w2 >> 31),
        nov=w3 & NA, d2=(w3 >> 8) & NA, d4=(w3 >> 16) & NA, d8=(w3 >> 24) & NA,
    )


def capture(ocd, telem_addr, seconds, records, hz, out):
    """Poll until `seconds` elapsed; append decoded records to `out`; return stats."""
    period = 1.0 / hz
    t_end = time.time() + seconds
    prev_seq = None
    first_seq = last_seq = 0
    lost = 0
    t0 = time.time()
    while time.time() < t_end:
        t_poll = time.time()
        ctrl = ocd.mdw(telem_addr, CTRL_WORDS)
        seq = ctrl[CTRL_SEQ]
        if ctrl[CTRL_MAGIC] != 0x544C4D31:
            sys.exit(f"bad telemetry magic 0x{ctrl[CTRL_MAGIC]:08x}: "
                     "firmware.elf does not match the device")
        if prev_seq is None or seq < prev_seq:  # start, or device rebooted
            first_seq = seq
            prev_seq = seq
        elif seq != prev_seq:
            ring = ocd.mdw(telem_addr + 4 * TELEMETRIES_OFFSET, records * 4)
            n = seq - prev_seq
            if n > records:  # ring overwrote: oldest n-records are gone
                lost += n - records
                n = records
            # Slot of record for seq s is s % records; the newest written is
            # (seq-1) % records, so walk back n records from there.
            for i in range(n - 1, -1, -1):
                s = seq - 1 - i
                base = (s % records) * 4
                rec = decode(s, ring[base:base + 4])
                rec["t"] = round(t_poll - t0, 3)
                out.append(rec)
            prev_seq = seq
        # Pace the loop: reads are cheap but the target's bus is not free.
        delay = period - (time.time() - t_poll)
        if delay > 0:
            time.sleep(delay)
    last_seq = prev_seq
    return dict(first=first_seq, last=last_seq, lost=lost,
                duration=time.time() - t0)


def summarize(ctrl, st, rows):
    dur = st["duration"]
    n = st["last"] - st["first"]
    fps = n / dur if dur > 0 else 0.0
    print(f"captured {n} frames in {dur:.1f}s ({fps:.1f} fps, "
          f"{st['lost']} lost to slow polling)")
    for name, word, fmt in [
            ("lock", CTRL_LASTLOCK, "frame {}"),
            ("unlock", CTRL_LASTUNLOCK, "frame {}"),
            ("anchor", CTRL_LASTANCHOR, "frame {}"),
            ("pattern", CTRL_LASTPATTERN, "frame {} -> pattern {}")]:
        v = ctrl[word]
        if not v:
            continue
        if name == "pattern":
            print(f"last {name}: {v & 0xFFFF} -> pattern {v >> 16}")
        else:
            print(f"last {name}: frame {v}")
    counts = {k: sum(r[k] for r in rows)
              for k in ("onset", "beat", "anchor", "pchange", "locked", "silent")}
    print("events: " + "  ".join(f"{k}={v}" for k, v in counts.items()))


def plot(rows, path):
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except ImportError:
        print("no matplotlib; skipping plot", file=sys.stderr)
        return
    t = [r["t"] for r in rows]
    fig, ax = plt.subplots(4, 1, sharex=True, figsize=(12, 8))
    ax[0].plot(t, [r["total"] for r in rows], lw=0.8)
    ax[0].set_ylabel("total\n(log2 Q8)")
    beats = [r["t"] for r in rows if r["beat"]]
    anchors = [r["t"] for r in rows if r["anchor"]]
    ax[0].vlines(beats, 0, 1, transform=ax[0].get_xaxis_transform(),
                 lw=0.5, color="tab:orange", alpha=0.6)
    ax[0].vlines(anchors, 0, 1, transform=ax[0].get_xaxis_transform(),
                 lw=1.2, color="tab:red")
    ax[1].plot(t, [r["period"] for r in rows], drawstyle="steps-post", lw=0.8)
    ax[1].set_ylabel("period\n(frames)")
    ax[1].plot(t, [r["votes"] for r in rows], lw=0.6, alpha=0.6)
    ax[2].plot(t, [r["phase"] for r in rows], drawstyle="steps-post", lw=0.8)
    ax[2].set_ylabel("PLL phase")
    ax[2].axhline(0, lw=0.5, color="gray")
    for key, color in [("nov", "tab:blue"), ("d2", "tab:green"),
                       ("d4", "tab:purple"), ("d8", "tab:brown")]:
        pts = [(r["t"], r[key]) for r in rows if r[key] != NA]
        if pts:
            xs, ys = zip(*pts)
            ax[3].plot(xs, ys, ".", ms=3, color=color, label=key)
    ax[3].set_ylabel("hash dist")
    ax[3].legend(fontsize=7, ncol=4)
    ax[3].set_xlabel("t (s)")
    # Lock band as a light background stripe on every axis.
    spans = []
    start = None
    for r in rows:
        if r["locked"] and start is None:
            start = r["t"]
        elif not r["locked"] and start is not None:
            spans.append((start, r["t"]))
            start = None
    if start is not None:
        spans.append((start, t[-1]))
    for a in ax:
        for s, e in spans:
            a.axvspan(s, e, color="green", alpha=0.07)
    fig.tight_layout()
    fig.savefig(path, dpi=110)
    print(f"plot -> {path}")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--elf", default=None, help="firmware.elf (default: ../firmware.elf)")
    ap.add_argument("--seconds", type=float, default=10)
    ap.add_argument("--hz", type=float, default=10, help="poll rate (default 10)")
    ap.add_argument("--play", metavar="SS:DUR", help="ffplay podcast segment during capture")
    ap.add_argument("--podcast", default=PODCAST_DEFAULT)
    ap.add_argument("--out", help="write CSV here")
    ap.add_argument("--plot", help="write PNG here")
    ap.add_argument("--peek", metavar="SYMBOL", help="print one control-block/symbol read and exit")
    ap.add_argument("--mark", action="store_true", help="stamp a host-mark record first")
    ap.add_argument("--quiet", action="store_true")
    args = ap.parse_args()

    import os
    here = os.path.dirname(os.path.abspath(__file__))
    elf = args.elf or os.path.join(here, "..", "firmware.elf")
    syms = resolve_symbols(elf)
    key = next((k for k in syms if k.endswith(".Telem")), None)
    if key is None:
        sys.exit(f"no syncdsp.Telem symbol in {elf} - not the debug build?")
    telem = syms[key]

    spawn_server(os.path.dirname(elf))
    ocd = OpenOCD()
    try:
        if args.peek:
            addr = syms.get(args.peek)
            if addr is None:  # accept short names like syncdsp.Telem
                matches = [(k, v) for k, v in syms.items()
                           if k.endswith("." + args.peek.lstrip("."))]
                if len(matches) != 1:
                    sys.exit(f"symbol {args.peek!r} not unique/present in {elf}")
                addr = matches[0][1]
            words = 10 if addr == telem else 4
            print(f"{args.peek} @0x{addr:08x}: " +
                  " ".join(f"{w:08x}" for w in ocd.mdw(addr, words)))
            return

        if args.mark:
            ocd.cmd(f"mww 0x{telem + 4*CTRL_HOSTMARK:08x} 1")

        player = None
        if args.play:
            ss, _, dur = args.play.partition(":")
            player = subprocess.Popen(
                ["ffplay", "-v", "error", "-nodisp", "-autoexit",
                 "-ss", ss, "-t", dur or str(args.seconds), args.podcast],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

        out = []
        st = capture(ocd, telem, args.seconds, RECORDS, args.hz, out)
        ctrl = ocd.mdw(telem, CTRL_WORDS)
        if player:
            player.wait()

        if args.out:
            fields = ["seq", "t", "hash", "onset", "beat", "bar", "anchor",
                      "pchange", "locked", "silent", "pushed", "mark",
                      "returned", "pattern", "votes", "period", "total", "phase",
                      "onset_age", "settled", "primed", "nov", "d2", "d4", "d8"]
            with open(args.out, "w", newline="") as f:
                w = csv.DictWriter(f, fieldnames=fields)
                w.writeheader()
                w.writerows(out)
            print(f"csv -> {args.out} ({len(out)} rows)")
        if args.plot:
            plot(out, args.plot)
        if not args.quiet:
            summarize(ctrl, st, out)
    finally:
        ocd.close()


if __name__ == "__main__":
    main()
