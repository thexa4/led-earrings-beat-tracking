package syncdsp

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
)

// Two earrings at a party are not at the same distance from the PA, and the
// question that matters is not "does a device 42 dB down match one at full
// scale" but "does the device on the left ear match the one on the right".
// Both are usually well below full scale, and both are usually within a few dB
// of each other - but a few dB was enough to disagree on the tempo before
// input normalization, so the whole grid gets compared against the whole grid.
//
// Set LEVEL_DUMP=prefix to also write the per-frame Hamming distance of the
// worst pair, which is how the "which musical parts are we worse at" profile
// gets made.

func attenuate(db int32) func([]uint16, *int32) {
	return func(w []uint16, _ *int32) {
		// Shifts rather than a float divide: 6 dB per step, so the rounding
		// matches what a real gain stage (or a quieter microphone) does.
		for i, v := range w {
			y := int32(v) - 32768
			for s := int32(0); s < db/6; s++ {
				y >>= 1
			}
			w[i] = uint16(y + 32768)
		}
	}
}

// levelDB are the input levels compared, in dB below full scale. ~48 dB is the
// bench operating point (a loud speaker at 1 cm reaches this microphone as a
// few tens of counts); further down the front end's own noise becomes the
// loudest thing in the window, which no amount of gain recovers.
var levelDB = []int32{0, 6, 12, 18, 24, 36, 48}

type trace struct {
	evs    []Events
	period []int32
	rms    int64
}

func runLevel(db int32, frames int) trace {
	in := newInst(attenuate(db), false)
	tr := trace{evs: make([]Events, frames), period: make([]int32, frames)}
	win := make([]uint16, Window)
	var acc int64
	for f := range frames {
		Synth(int64(f*Window), win)
		attenuate(db)(win, nil)
		for _, v := range win {
			d := int32(v) - 32768
			acc += int64(d) * int64(d)
		}
		tr.evs[f] = in.p.Process(win)
		tr.period[f] = in.p.BeatPeriod()
	}
	tr.rms = int64(math.Sqrt(float64(acc) / float64(frames*Window)))
	return tr
}

func TestLevelMatrix(t *testing.T) {
	runs := make([]trace, len(levelDB))
	for i, db := range levelDB {
		runs[i] = runLevel(db, testFrames)
	}

	t.Logf("%-6s %8s %7s %7s", "dB", "rms", "period", "lock%")
	for i, db := range levelDB {
		locked, n := 0, 0
		for _, e := range runs[i].evs[WarmupFrames:] {
			if e.Locked {
				locked++
			}
			n++
		}
		t.Logf("-%-5d %8d %7d %6.1f", db, runs[i].rms,
			median32(runs[i].period[WarmupFrames:]), 100*float64(locked)/float64(max(n, 1)))
	}

	worstPair, worstAgree := "", 1.0
	hdr := strings.Builder{}
	for _, db := range levelDB {
		fmt.Fprintf(&hdr, "%7s", fmt.Sprintf("-%ddB", db))
	}
	t.Logf("\npattern agreement (row A vs column B), over frames where A is locked:")
	t.Logf("%-7s%s", "", hdr.String())
	for i := range levelDB {
		row := strings.Builder{}
		for j := range levelDB {
			same, tot := agree(runs[i], runs[j], func(a, b Events) bool { return a.Pattern == b.Pattern })
			a := float64(same) / math.Max(float64(tot), 1)
			if i != j && a < worstAgree {
				worstAgree, worstPair = a, fmt.Sprintf("-%d/-%d dB", levelDB[i], levelDB[j])
			}
			fmt.Fprintf(&row, "%6.0f%% ", 100*a)
		}
		t.Logf("-%-6d%s", levelDB[i], row.String())
	}
	t.Logf("\nexact hash agreement, same pairs:")
	t.Logf("%-7s%s", "", hdr.String())
	for i := range levelDB {
		row := strings.Builder{}
		for j := range levelDB {
			same, tot := agree(runs[i], runs[j], func(a, b Events) bool { return a.Hash == b.Hash })
			fmt.Fprintf(&row, "%6.0f%% ", 100*float64(same)/math.Max(float64(tot), 1))
		}
		t.Logf("-%-6d%s", levelDB[i], row.String())
	}

	// The claim the normalizer exists to support: any two devices that both
	// hear the music clearly make the same pattern decision most of the time.
	for i := range levelDB {
		for j := range levelDB {
			if levelDB[i] > 36 || levelDB[j] > 36 || i == j {
				continue
			}
			same, tot := agree(runs[i], runs[j], func(a, b Events) bool { return a.Pattern == b.Pattern })
			if a := float64(same) / math.Max(float64(tot), 1); a < 0.7 {
				t.Errorf("pattern agreement -%d dB vs -%d dB: %.0f%% < 70%%",
					levelDB[i], levelDB[j], 100*a)
			}
		}
	}
	t.Logf("worst off-diagonal pattern pair: %s at %.0f%%", worstPair, 100*worstAgree)

	if p := os.Getenv("LEVEL_DUMP"); p != "" {
		i, j := 0, len(levelDB)-1
		f, err := os.Create(p + ".csv")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		fmt.Fprintf(f, "frame,dist,locked,anchor,beat,pa,pb\n")
		for k := range runs[i].evs {
			if k < int(WarmupFrames) {
				continue
			}
			fmt.Fprintf(f, "%d,%d,%d,%d,%d,%d,%d\n", k,
				hashDist(runs[i].evs[k].Hash, runs[j].evs[k].Hash),
				b2i(runs[i].evs[k].Locked), b2i(runs[i].evs[k].Anchor), b2i(runs[i].evs[k].Beat),
				runs[i].period[k], runs[j].period[k])
		}
	}
}

// agree counts frames where A is locked and pred(a,b) holds.
func agree(a, b trace, pred func(Events, Events) bool) (same, tot int) {
	for f := range a.evs {
		if f < int(WarmupFrames) || !a.evs[f].Locked {
			continue
		}
		tot++
		if pred(a.evs[f], b.evs[f]) {
			same++
		}
	}
	return
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func median32(v []int32) int32 {
	var s []int32
	for _, x := range v {
		if x > 0 {
			s = append(s, x)
		}
	}
	if len(s) == 0 {
		return 0
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}
