#!/usr/bin/env python3
"""Place each earring's beats on the kick reference of the played audio.

  tools/synckick.py RUNDIR WAV BEATS SS

e.g. a run of syncpoll.py --ss 2400 against a 16 kHz excerpt made with
  ffmpeg -ss 2400 -t 600 -i podcast.mp3 -ac 1 -ar 16000 pod2400.wav
  python tools/beatref/kbeats.py pod2400.wav pod2400.beats
scored with: tools/synckick.py RUN pod2400.wav pod2400.beats 2400

WAV is the 16 kHz excerpt that starts at podcast second SS' (its .beats from
tools/beatref/kbeats.py); the run played the podcast from SS (play.txt). Host
time -> audio time is found per earring by cross-correlating the earring's
`bass` (log level, per poll) with the WAV's log energy below 250 Hz on a
1/64 s grid; the two earrings must come out with about the same latency (one
speaker), which is the check on the alignment. The latency includes the
pipeline's own delay, so a beat aligned this way is where the earring put it
relative to what it heard.
"""
import sys
import wave

import numpy as np

import syncpair as p

FR = 64.0


def wav_env(path):
    w = wave.open(path)
    sr = w.getframerate()
    x = np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16).astype(float)
    hop = int(sr / FR)
    n = len(x) // hop
    X = np.fft.rfft(x[: n * hop].reshape(n, hop) * np.hanning(hop), axis=1)
    f = np.fft.rfftfreq(hop, 1 / sr)
    e = (np.abs(X[:, (f > 30) & (f < 250)]) ** 2).sum(1)
    return np.log2(e + 1e3), sr


def main():
    run, wavp, beatp, ss_wav = sys.argv[1], sys.argv[2], sys.argv[3], float(sys.argv[4])
    t_play, _, ss = open(f"{run}/play.txt").read().split()
    t_play, ss = float(t_play), float(ss.split("=")[1])
    env, _ = wav_env(wavp)
    kicks = np.loadtxt(beatp)
    off = ss - ss_wav  # audio second of the run's start, in WAV time
    res = {}
    for s in "LR":
        d = p.load(f"{run}/{s}.csv")
        tm = (d["t0"] + d["t1"]) / 2
        rate = np.polyfit(tm, d["frame"], 1)[0]
        g = np.arange(tm[0], tm[-1], 1 / FR)
        dev = np.interp(g, tm, d["bass"])
        dev = dev - np.convolve(dev, np.ones(64) / 64, "same")
        best = None
        for lat in np.arange(0.0, 6.0, 1 / FR):
            a = (g - t_play - lat) + off  # WAV time of each grid point
            k = np.round(a * FR).astype(int)
            ok = (k >= 0) & (k < len(env)) & (g > tm[0] + 20)
            e = env[k[ok]]
            e = e - np.convolve(e, np.ones(64) / 64, "same")
            c = np.corrcoef(dev[ok], e)[0, 1]
            if best is None or c > best[0]:
                best = (c, lat)
        c, lat = best
        bt = p.beats(d, rate)
        bt = bt[bt > tm[0] + 20]
        a = (bt - t_play - lat) + off
        a = a[(a > kicks[0]) & (a < kicks[-1])]
        j = np.clip(np.searchsorted(kicks, a), 1, len(kicks) - 1)
        per = kicks[j] - kicks[j - 1]
        ph = (a - kicks[j - 1]) / per
        near = np.minimum(a - kicks[j - 1], kicks[j] - a)
        hist = np.histogram((ph * 8 + 0.5) % 8, bins=8, range=(0, 8))[0]
        res[s] = (lat, c)
        print(f"{s}: latency {lat * 1e3:.0f} ms (corr {c:.2f}); beats within 70 ms of a kick "
              f"{np.mean(near < 0.07):.0%}; phase in kick grid (bin 0 = on the kick): {hist}")
    print(f"latency difference L-R {(res['L'][0] - res['R'][0]) * 1e3:+.0f} ms")


if __name__ == "__main__":
    main()
