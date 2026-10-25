package syncdsp

import (
	"sort"
	"testing"
)

// TestDbgActivity measures the absolute activity statistic - the mean local
// movement in ADC codes - on a silent room and on the test signal at a range of
// attenuations, on both bench models. A gate needs a threshold above the room on
// the noisier device and below the signal at the quiet end.
func TestDbgActivity(t *testing.T) {
	const frames = 8 * synthSection / Window
	pc := func(v []int32, q float64) float64 {
		s := append([]int32{}, v...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return float64(s[int(q*float64(len(s)-1))]) / 256
	}
	cases := []struct {
		name string
		db   int32
		idle bool
	}{
		{"L idle", 0, true}, {"R idle", 0, true},
	}
	for db := int32(12); db <= 72; db += 6 {
		cases = append(cases,
			struct {
				name string
				db   int32
				idle bool
			}{"L" + itoa(int(db)), db, false},
			struct {
				name string
				db   int32
				idle bool
			}{"R" + itoa(int(db)), db, false})
	}
	for _, c := range cases {
		var fe func([]uint16, *int32)
		switch {
		case c.idle && c.name == "L idle":
			fe = benchL(0)
		case c.idle:
			fe = benchR(0)
		case c.name[0] == 'L':
			fe = benchL(c.db)
		default:
			fe = benchR(c.db)
		}
		in := newInst(fe, false)
		win := make([]uint16, Window)
		var act, pk []int32
		for f := range frames {
			for i := range win {
				win[i] = 32768
			}
			if !c.idle {
				Synth(int64(f*Window), win)
			}
			fe(win, &in.carry)
			in.p.Process(win)
			if f > 4*1<<ModPole {
				act = append(act, in.p.Activity())
				pk = append(pk, in.p.InputPeak())
			}
		}
		t.Logf("%-8s activity p10 %6.1f p50 %6.1f p90 %6.1f codes   agcPeak p50 %7.0f",
			c.name, pc(act, .1), pc(act, .5), pc(act, .9), float64(pctile50(pk)))
	}
}

func pctile50(v []int32) int32 {
	s := append([]int32{}, v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}
