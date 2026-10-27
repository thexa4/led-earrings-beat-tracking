# Kick-based beat reference for syncdsp/beat_eval_test.go (BEATREFDIR):
# librosa's dynamic-programming beat tracker on an onset envelope from the mel
# bands below --fmax only, so the beat lands on the kick rather than on hats or
# claps.
#
#   python kbeats.py in.wav out.beats [--fmax 200] [--bpm 120] [--compare full.beats]
#
# --compare prints where another beat list (e.g. librosa's full-band
# beat_track) falls in the kick grid, as an 8-bin phase histogram: on the EDM
# set this was made for, full-band beats sit at bin 4 (the off-beat) about
# half the time. For a loudspeaker recording with little sub-bass, raise
# --fmax (300-500) and give --bpm.
import argparse

import librosa
import numpy as np

ap = argparse.ArgumentParser()
ap.add_argument("wav")
ap.add_argument("out")
ap.add_argument("--fmax", type=float, default=200)
ap.add_argument("--bpm", type=float, default=120)
ap.add_argument("--compare")
a = ap.parse_args()

y, sr = librosa.load(a.wav, sr=None, mono=True)
hop = 128
oenv = librosa.onset.onset_strength(y=y, sr=sr, hop_length=hop, n_fft=2048, n_mels=64, fmax=a.fmax)
tempo, beats = librosa.beat.beat_track(onset_envelope=oenv, sr=sr, hop_length=hop, units="time",
                                       tightness=400, start_bpm=a.bpm)
np.savetxt(a.out, beats, fmt="%.4f")
print("%s: %.1f BPM, %d beats" % (a.wav, np.atleast_1d(tempo)[0], len(beats)))

if a.compare:
    other = np.loadtxt(a.compare)
    j = np.searchsorted(beats, other)
    ok = (j > 0) & (j < len(beats))
    ph = (other[ok] - beats[j[ok] - 1]) / (beats[j[ok]] - beats[j[ok] - 1])
    print("phase of %s in the kick grid (bin 0 = on the kick): %s"
          % (a.compare, np.histogram((ph * 8 + 0.5) % 8, bins=8, range=(0, 8))[0]))
