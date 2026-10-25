package syncdsp

import (
	"fmt"
	"testing"
)

// TestQuietFeature is a diagnostic, not a requirement.
//
// The quiet agreement cases fail, and the diagnostic that found the mechanism shows
// why: at -42 dB the AGC applies its full 64x, which lifts the front end's own noise
// into the same range as the music. That noise is roughly flat across the bands, while
// the music is not, so it compresses the spectrum's contrast - measured as the treble
// band sitting 89 Q8 units higher relative to its device's own mean on the amplified
// side than on the clean one, and the bass 42 lower. A zero-weighted-sum centroid
// cancels a *common* offset and cannot cancel that, so the two devices land on
// different centroids and therefore different colours.
//
// The question this answers is whether any scalar feature survives that, because if
// none does, the fix is upstream of the feature - either the AGC must stop amplifying
// noise, or the pipeline must refuse to decide - and tuning weights or bucket cuts is
// busywork.
//
// Each candidate is scored against the clean device over the same audio. The last
// column is the useful number.
func TestQuietFeature(t *testing.T) {
	cands := []struct {
		name string
		f    feature
	}{
		{"ramp", rampFeature},
		{"centroid", centroidFeature},
		{"reliable", reliableFeature},
		{"interior", interiorFeature},
	}
	devs := []struct {
		name string
		dist func([]uint16, *int32)
	}{
		{"clean", nil},
		{"quiet-42", quiet},
		{"quiet+dc", benchOffset},
	}
	const frames = 2600
	trs := make([]runTrace, len(devs))
	for i, d := range devs {
		in := newInst(d.dist, false)
		win := make([]uint16, Window)
		for f := WarmupFrames + PatternWarmupFrames; f < frames; f++ {
			Synth(int64(f*Window), win)
			if d.dist != nil {
				d.dist(win, &in.carry)
			}
			in.p.Process(win)
			trs[i].smooth = append(trs[i].smooth, in.p.smoothSnap)
		}
	}
	for _, c := range cands {
		pats := make([][]int32, len(trs))
		for i := range trs {
			pats[i] = patternAt(trs[i], c.f)
		}
		line := fmt.Sprintf("%-9s", c.name)
		for i := 1; i < len(trs); i++ {
			_, k := agreementOf(pats[0], pats[i])
			line += fmt.Sprintf(" %s %5.1f%%/%+0.2f", devs[i].name, 100*agreeOf(pats[0], pats[i]), k)
		}
		t.Log(line)
	}
}
