package syncdsp

// Display levels (Pipeline.Level). They feed only the animations and uploaded
// programs, so the checks are about looks: the full range is used, two
// earrings with different microphone gains show nearly the same thing, the
// bass level follows the kick, and silence is dark.
//
//	LEVELWAV=../../hears-left.wav go test -run TestLevelsWav -v ./syncdsp/
//
// prints per-band level statistics for a real recording (16 kHz mono PCM16).

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// levelRun feeds frames of the synth through dist (nil: undistorted) and
// returns every frame's levels and events.
func levelRun(frames int, dist func([]uint16, *int32)) ([][NBands + 1]uint8, []Events) {
	var p Pipeline
	p.Init()
	win := make([]uint16, Window)
	var carry int32
	lv := make([][NBands + 1]uint8, frames)
	evs := make([]Events, frames)
	for f := range frames {
		Synth(int64(f)*Window, win)
		if dist != nil {
			dist(win, &carry)
		}
		evs[f] = p.Process(win)
		lv[f] = p.Level
	}
	return lv, evs
}

func TestLevelsGainInvariant(t *testing.T) {
	const frames = 64 * 40
	a, _ := levelRun(frames, nil)
	b, _ := levelRun(frames, halfGain)
	// After the peaks have settled, a 6 dB gain difference must move the
	// levels by little: the reference peak moves with the gain.
	for i := range NBands + 1 {
		var sum, n float64
		for f := 64 * 10; f < frames; f++ {
			d := float64(a[f][i]) - float64(b[f][i])
			if d < 0 {
				d = -d
			}
			sum += d
			n++
		}
		if m := sum / n; m > 16 {
			t.Errorf("level %d: mean |A-B| %.1f of 255 between full and half gain", i, m)
		}
	}
}

func TestLevelsSilence(t *testing.T) {
	var p Pipeline
	p.Init()
	win := make([]uint16, Window)
	// Music first, so the peaks are up, then a long stretch of the bias alone.
	for f := range 64 * 10 {
		Synth(int64(f)*Window, win)
		p.Process(win)
	}
	for i := range win {
		win[i] = benchDC
	}
	for range 64 * 2 {
		p.Process(win)
	}
	for i, v := range p.Level {
		if v != 0 {
			t.Errorf("level %d = %d in silence", i, v)
		}
	}
}

func TestLevelsWav(t *testing.T) {
	path := os.Getenv("LEVELWAV")
	if path == "" {
		t.Skip("set LEVELWAV to a 16 kHz mono PCM16 WAV")
	}
	src := readPCM16(t, path)
	var p Pipeline
	p.Init()
	win := make([]uint16, Window)
	var lv [][NBands + 1]uint8
	var beats []bool
	for n := 0; (n+1)*Window <= len(src); n++ {
		for i := range win {
			win[i] = uint16(int32(src[n*Window+i]) + benchDC)
		}
		ev := p.Process(win)
		lv = append(lv, p.Level)
		beats = append(beats, ev.Beat && ev.Locked)
	}
	skip := 64 * 5
	names := []string{">4k", "2.5k", "1.6k", "1k", "630", "400", "250", "<250", "all"}
	for i := range NBands + 1 {
		var vs []int
		for _, l := range lv[skip:] {
			vs = append(vs, int(l[i]))
		}
		sort.Ints(vs)
		q := func(f float64) int { return vs[int(f*float64(len(vs)-1))] }
		// Mean level on beat frames against all frames: a level that follows
		// the kick is higher on the beat.
		var onB, all float64
		var nB int
		for f := skip; f < len(lv); f++ {
			all += float64(lv[f][i])
			if beats[f] {
				onB += float64(lv[f][i])
				nB++
			}
		}
		all /= float64(len(lv) - skip)
		if nB > 0 {
			onB /= float64(nB)
		}
		t.Logf("%-5s p10 %3d p50 %3d p90 %3d  mean %5.1f  on beat %5.1f (%d beats)",
			names[i], q(0.1), q(0.5), q(0.9), all, onB, nB)
	}
	// A coarse strip chart of the bass and overall levels, one character per
	// frame, for eyeballing against the beat marks.
	var sb strings.Builder
	for f := 64 * 30; f < 64*32 && f < len(lv); f++ {
		c := " .:-=+*#%@"[int(lv[f][NBands-1])*9/255]
		if beats[f] {
			c = '|'
		}
		sb.WriteByte(c)
	}
	t.Log("bass 30-32 s: " + sb.String())
}
