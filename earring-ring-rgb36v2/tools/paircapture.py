#!/usr/bin/env python3
"""Capture telemetry from BOTH earrings (on the remote flashing host) while
this machine plays audio into the room.

  tools/paircapture.py --file /tmp/psync/h900.wav [--outdir DIR] [--stop]

The earrings hang off the pi (root), one clone ST-Link each. The probes
share a garbage serial, so each server is pinned to its USB port with
`adapter usb location <busid>` (see tools/flash.sh in the repo root).
The servers stay up between runs; --stop tears them down.

Telnet ports on the pi: 4445 = left (1-1.1), 4446 = right (1-1.2),
forwarded to localhost with an ssh -L tunnel.

Audio plays HERE (ffplay), not on the pi. The script never touches the
output volume. Feed it the ORIGINAL full-rate audio (podcast.mp3 or a
full-rate WAV): a 16 kHz mono file sounds awful through speakers. The
16 kHz versions (/tmp/psync/h900.wav, full.wav) exist only for the host
replay tests, whose pipeline is fixed at 16 kSPS - never play those.

Writes OUT/L.csv and OUT/R.csv (same format as capture2.py), then scores
them with devpair.py and fieldwise.py. With --state it also writes
OUT/state_L.csv / state_R.csv: the pattern selector's live centroid, its
reference and its spread per poll, which is what makes a long capture
reusable - see PipeState.
"""
import argparse
import csv
import os
import struct
import subprocess
import sys
import threading
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import telem

HOST_DEFAULT = "pi"
PORTS = {"L": ("1-1.1", 4445), "R": ("1-1.2", 4446)}


def ssh_cmd(host, cmd):
    return subprocess.run(["ssh", "-o", "BatchMode=yes", host, cmd],
                          capture_output=True, text=True, timeout=30)


def stop_servers(host):
    # plain `systemctl stop` waits (60 s+) for openocd to release the probe;
    # KILL is immediate and the transient units clean themselves up.
    for name in PORTS:
        ssh_cmd(host, f"systemctl kill -s KILL oo-{name}.service 2>/dev/null; true")
    ssh_cmd(host, "sleep 1; pkill -x openocd; systemctl reset-failed 'oo-*' 2>/dev/null; true")


def server_alive(tport):
    """A server is alive only if its telnet answers: unbinding its probe to
    start the OTHER server kills the first one's USB handle, so we verify
    after every spawn and restart what died."""
    try:
        oc = telem.OpenOCD("localhost", tport)
        oc.cmd("version")
        oc.close()
        return True
    except Exception:
        return False


def spawn_servers(host):
    """One persistent openocd per probe, pinned by USB location."""
    if all(server_alive(t) for _, t in PORTS.values()):
        print("servers already up")
        return
    stop_servers(host)
    time.sleep(1)
    for name, (port_path, tport) in PORTS.items():
        # systemd-run: a plain `openocd ... &` over ssh keeps the ssh
        # session open (sshd waits for the whole cgroup's session), which
        # hangs ssh_cmd. A transient unit detaches cleanly and stops with
        # `systemctl stop oo-<name>.service`.
        ssh_cmd(host,
                f"systemd-run --unit=oo-{name} --collect bash -c "
                f"'cd /tmp && exec openocd -c \"adapter usb location {port_path}\" "
                f"-f interface/stlink.cfg -f target/stm32l0.cfg "
                f"-c \"adapter speed 1000\" "
                f"-c \"telnet_port {tport}\" -c \"gdb_port {tport - 1111}\" "
                f"-c \"tcl_port {tport + 2222}\" "
                f"-c init -c \"reset run\" "
                f"> /tmp/oo-{name}.log 2>&1'")
    for name, (_, tport) in PORTS.items():
        for _ in range(30):
            if server_alive(tport):
                break
            time.sleep(1)
        else:
            log = ssh_cmd(host, f"tail -5 /tmp/oo-{name}.log").stdout
            raise RuntimeError(f"{name}: server did not start: {log}")
        print(f"server {name} up on {host}:{tport}")


def open_tunnel(host, ports):
    # A tunnel leaked from an interrupted earlier run would hold the local
    # ports; kill matching ones first. Match on the forwarded port rather than on
    # our own option string: a hand-made `ssh -f -N -L 4445:...` from an earlier
    # debugging pass holds the port just as firmly, and then ssh exits here with
    # "Address already in use" while the capture happily reads through the
    # stranger's tunnel - which has no keepalive and no owner, so it can quietly
    # die an hour into a long run.
    fwd_ports = "|".join(str(p) for _, p in ports)
    subprocess.run(["pkill", "-f", f"ssh .*-L ({fwd_ports}):"])
    time.sleep(0.5)
    fwd = " ".join(f"-L {p}:localhost:{p}" for _, p in ports)
    return subprocess.Popen(
        f"ssh -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 "
        f"-N -n {fwd} {host}", shell=True)


class PipeState:
    """The pattern selector's own state, read from main.syncPipe once per poll.

    The telemetry ring carries the committed pattern per analysis frame but not the
    three numbers the choice is made of - the centroid, its running reference and its
    spread - and those exist only live, so they are sampled per poll into a file of
    their own beside the capture rather than per telemetry row. That is what makes a
    long paired capture worth more than a score: with the two trajectories on one time
    axis, a candidate selector can be replayed offline against the same minute of real
    audio in both earrings, instead of costing a flash and a capture per idea.

    Offsets come from the ELF's DWARF, never hardcoded - see patternstate, which owns
    that parser for the same reason.
    """

    # centGapQ8 is the low-passed deviation the selector actually decides on. It is
    # recorded because without it a replay has to simulate that low-pass from the raw
    # centroid, and then the one quantity whose tuning was chosen offline is the one
    # quantity never observed on a device running the code that uses it - every
    # comparison is against a device running something older.
    NAMES = {"cent", "centGapQ8", "centAvg", "centDev", "smooth", "agcShift",
             "frame", "lastPattern"}
    FIELDS = ["t", "cent", "gapQ8", "centAvg", "centDev", "pat", "frame",
              "agc"] + [f"b{i}" for i in range(8)]

    # How many polls to watch before deciding the offsets are wrong.
    CHECK_SAMPLES = 25

    def __init__(self, elf):
        import patternstate
        self.off = patternstate.member_offsets(elf, self.NAMES)
        missing = self.NAMES - set(self.off)
        if missing:
            raise SystemExit(f"{elf}: DWARF has no Pipeline member(s) "
                             f"{sorted(missing)} - stale elf?")
        syms = telem.resolve_symbols(elf)
        self.addr = syms[next(k for k in syms if k.endswith("main.syncPipe"))]
        self.words = max(self.off.values()) // 4 + 2
        self._n = 0
        self._advances = False
        self._prev_frame = None
        self._bad = []

    def _check(self, row):
        """Stop early if the ELF does not describe the image on the device.

        A stale ELF passes this class's own constructor check: it asks DWARF for
        member offsets and stale DWARF answers cheerfully, just four bytes wrong.
        Inserting one field into Pipeline shifts every member after it - centGapQ8
        went in at 1012 and took centAvg, centDev and frame with it - and the
        output is not an exception and not obviously nonsense. The band energies
        sit before the shift and stay exactly right, `pat` still lands in 0..3, and
        the file fills up at the expected rate, so a whole hour gets recorded and
        then scored into a plausible number computed from the wrong columns. It
        measured 63% agreement between two earrings that were never being read.

        So check two things that only the correct words can satisfy, both true by
        construction in the firmware: `frame` counts analysis frames at 62.5 Hz and
        cannot sit still on a running device, and `centDev` is an exponential
        average of an absolute value and so cannot be negative. Caught in the
        first few seconds, which is the point: the alternative is finding out after
        the hour.
        """
        self._n += 1
        if row["centDev"] < 0 and not any("centDev" in b for b in self._bad):
            self._bad.append(
                f"centDev read {row['centDev']}, negative: it is an average of an "
                f"absolute value and cannot be")
        if self._prev_frame is not None and row["frame"] > self._prev_frame:
            self._advances = True
        self._prev_frame = row["frame"]
        if self._n != self.CHECK_SAMPLES:
            return
        if not self._advances:
            self._bad.append(
                f"frame never advanced from {self._prev_frame} over "
                f"{self.CHECK_SAMPLES} polls: it counts analysis frames")
        if self._bad:
            raise SystemExit(
                "state read is inconsistent with the firmware:\n  " +
                "\n  ".join(self._bad) +
                "\nThe ELF almost certainly does not match the image on the "
                f"device.\nRebuild both from the same tree and prove it before "
                "capturing:\n"
                "  tinygo build -o firmware.elf -target=./stm32l0x1.json "
                "-opt=2 -panic=trap .\n"
                "  objcopy -O ihex firmware.elf /tmp/check.hex   # must equal "
                "the flashed hex\n")

    def sample(self, ocd, t):
        w = ocd.mdw(self.addr, self.words)

        def g(name):
            return struct.unpack("<i", struct.pack("<I", w[self.off[name] // 4]))[0]

        row = dict(t=round(t, 3), cent=g("cent"), gapQ8=g("centGapQ8"),
                   centAvg=g("centAvg"), centDev=g("centDev"),
                   pat=g("lastPattern"), frame=g("frame"), agc=g("agcShift"))
        base = self.off["smooth"] // 4
        for i in range(8):
            row[f"b{i}"] = struct.unpack("<i",
                                         struct.pack("<I", w[base + i]))[0]
        self._check(row)
        return row


def capture(name, tport, elf, out, seconds, player, state_out=None):
    """Poll one device until `seconds` have passed.

    `player` is the already-running ffplay, or None. It is NOT started here:
    this function runs once per device, and starting it here - which is what this
    code used to do - played the same file twice, one ffplay per device thread,
    with a random fraction of a second between them. Two sources broadcasting the
    same signal a few centimetres apart is a comb filter, and the two earrings sit
    at different points in it, so they hear measurably different spectra from the
    same music. Every paired capture taken before this was fixed is a measurement
    of that interference pattern, not of the pipeline: it is the one explanation
    for the earrings' disagreement that the numbers actually supported, and it is
    why the paired recordings correlate at 0.03 where two microphones in one room
    should correlate near 1.

    Playback belongs to the caller, which starts exactly one player for the run.
    Never touch the output volume.
    """
    syms = telem.resolve_symbols(elf)
    taddr = syms[next(k for k in syms if k.endswith(".Telem"))]
    adc = syms[next(k for k in syms if k.endswith("main.adcData"))]
    ocd = telem.OpenOCD("localhost", tport)
    ocd.cmd("halt")
    time.sleep(0.05)
    ocd.cmd("resume")
    time.sleep(0.05)

    # Optional, and only for a long run: the selector's live state per poll. Built
    # before the loop so a stale elf fails immediately rather than after an hour.
    #
    # Written as it goes, and flushed every couple of seconds, because the runs this
    # exists for are measured in tens of minutes: a buffer kept until the end throws
    # away an hour of capture over one dropped connection at minute fifty-five. The
    # file's last row doubles as the liveness check for whoever started it.
    pipe = statef = statewr = None
    nstate = 0
    if state_out:
        pipe = PipeState(elf)
        statef = open(state_out, "w", newline="")
        statewr = csv.DictWriter(statef, fieldnames=PipeState.FIELDS)
        statewr.writeheader()
    rows, mic = [], []
    prev_seq = None
    fails = 0
    t0 = time.time()
    next_report = 300.0
    while time.time() - t0 < seconds:
        try:
            ctrl = ocd.mdw(taddr, telem.CTRL_WORDS)
            if pipe:
                statewr.writerow(pipe.sample(ocd, time.time() - t0))
                nstate += 1
                if nstate % 10 == 0:
                    statef.flush()
        except Exception as err:
            # Over a run measured in tens of minutes a probe hiccup is normal and
            # losing the whole capture is not. Reconnect, and only give up if the
            # device stays unreachable: 20 polls is several seconds of silence.
            fails += 1
            if fails == 1:
                print(f"{name}: poll failed ({err}), reconnecting", flush=True)
            try:
                ocd.close()
            except Exception:
                pass
            time.sleep(1)
            try:
                ocd = telem.OpenOCD("localhost", tport)
            except Exception:
                if fails > 20:
                    print(f"{name}: unreachable for 20 polls, stopping early",
                          flush=True)
                    break
                continue
            fails = 0
            prev_seq = None
            continue
        fails = 0
        seq = ctrl[telem.CTRL_SEQ]
        w = np.array(ocd.mdw(adc, 125), dtype=np.uint32)
        s = np.concatenate([(w & 0xFFFF), (w >> 16)]).astype(np.float64)
        s -= s.mean()
        now = time.time() - t0
        mic.append((now, seq, float(s.std())))
        if prev_seq is None or seq < prev_seq:
            prev_seq = seq
        elif seq != prev_seq:
            n = min(seq - prev_seq, telem.RECORDS)
            ring = ocd.mdw(taddr + 4 * telem.TELEMETRIES_OFFSET,
                           telem.RECORDS * 4)
            for i in range(n - 1, -1, -1):
                sq = seq - 1 - i
                rec = telem.decode(sq, ring[(sq % telem.RECORDS) * 4:
                                            (sq % telem.RECORDS) * 4 + 4])
                rec["t"] = round(now, 3)
                rows.append(rec)
            prev_seq = seq
        if state_out and time.time() - t0 >= next_report:
            print(f"{name}: {time.time() - t0:.0f} s, {nstate} state samples, "
                  f"{len(rows)} telemetry rows", flush=True)
            next_report += 300
    ocd.close()
    if state_out:
        statef.close()
        print(f"{state_out}: {nstate} state samples")
    fields = ["kind", "seq", "t", "hash", "onset", "beat", "bar", "anchor",
              "pchange", "locked", "silent", "pushed", "mark", "returned",
              "pattern", "votes", "period", "total", "phase", "onset_age",
              "settled", "primed", "nov", "d2", "d4", "d8", "rms"]
    with open(out, "w", newline="") as f:
        wr = csv.DictWriter(f, fieldnames=fields)
        wr.writeheader()
        for r in rows:
            row = dict(kind="T", rms="")
            row.update(r)
            wr.writerow(row)
        for t, s_, r_ in mic:
            wr.writerow(dict(kind="M", seq=s_, t=round(t, 4), rms=round(r_, 1)))
    print(f"{out}: {len(rows)} telemetry rows, {len(mic)} mic samples")


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default=HOST_DEFAULT)
    ap.add_argument("--file", help="WAV to play here (never changes volume)")
    ap.add_argument("--seconds", type=float, default=0)
    ap.add_argument("--lead", type=float, default=4.0,
                    help="seconds of capture before playback starts")
    ap.add_argument("--outdir", default="/tmp/psync/pair")
    ap.add_argument("--elf", default=os.path.join(here, "..", "firmware.elf"))
    ap.add_argument("--stop", action="store_true",
                    help="tear down the remote servers and exit")
    ap.add_argument("--no-play", action="store_true",
                    help="capture only (e.g. room is already playing music)")
    ap.add_argument("--state", action="store_true",
                    help="also record the pattern selector's live state per poll "
                         "to OUT/state_<side>.csv, for offline replay")
    args = ap.parse_args()

    if args.stop:
        stop_servers(args.host)
        print("servers stopped, adapters rebound")
        return

    os.makedirs(args.outdir, exist_ok=True)
    tunnel = open_tunnel(args.host, PORTS.values())
    time.sleep(1)
    try:
        spawn_servers(args.host)
        seconds = args.seconds or (75 + args.lead)
        # Exactly one player for the whole run, started once the capture threads
        # are up. See capture(): one ffplay per device meant two sources.
        time.sleep(args.lead)
        player = None
        if not args.no_play and args.file:
            player = subprocess.Popen(
                ["ffplay", "-v", "error", "-nodisp", "-autoexit", args.file],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        ths = []
        for name, (_, tport) in PORTS.items():
            t = threading.Thread(
                target=capture,
                args=(name, tport, args.elf,
                      os.path.join(args.outdir, f"{name}.csv"),
                      seconds - args.lead, player,
                      os.path.join(args.outdir, f"state_{name}.csv")
                      if args.state else None))
            t.start()
            ths.append(t)
        for t in ths:
            t.join()
        if player:
            try:
                player.wait(timeout=10)
            except subprocess.TimeoutExpired:
                player.kill()
    finally:
        tunnel.kill()

    L = os.path.join(args.outdir, "L.csv")
    R = os.path.join(args.outdir, "R.csv")
    for tool in ("devpair.py", "fieldwise.py"):
        print(f"===== {tool} =====")
        subprocess.run([sys.executable, os.path.join(here, tool), L, R])


if __name__ == "__main__":
    main()
