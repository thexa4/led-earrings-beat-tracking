package syncdsp

import (
	"math"
	"os"
	"sort"
	"testing"
)

// TestScratchRatio collects, sample by sample, the ratio between a sample's
// distance from its five-point median and the same distance measured from the four
// neighbours around it. That is the statistic the limiter decides on, so this is
// the distribution the two thresholds have to be placed in.
//
// The question is whether the tail of the music's own distribution reaches into
// the impulses'. Reported in "times the neighbourhood median", as percentiles over
// the whole run, plus the largest value seen.
func TestScratchRatio(t *testing.T) {
	const frames = 4 * synthSection / Window

	// The filter, duplicated: the point of this test is to see the statistic the
	// filter computes, so it must not be filtered first.
	ratios := func(win []uint16, acc *[]float64) {
		var x1, x2, x3, x4 int32
		primed := false
		for _, v := range win {
			x := int32(v)
			if !primed {
				primed = true
				x1, x2, x3, x4 = x, x, x, x
				continue
			}
			a, b, c, d, e := x1, x2, x3, x4, x
			if a > b {
				a, b = b, a
			}
			if d > e {
				d, e = e, d
			}
			if c > e {
				c, e = e, c
			}
			if c > d {
				c, d = d, c
			}
			if a > d {
				a, d = d, a
			}
			if a > c {
				a, c = c, a
			}
			if b > e {
				b, e = e, b
			}
			if b > d {
				b, d = d, b
			}
			if b > c {
				b, c = c, b
			}
			m := c
			dev := func(v int32) int32 {
				if v > m {
					return v - m
				}
				return m - v
			}
			d1, d2, d3, d4 := dev(x1), dev(x2), dev(x3), dev(x4)
			if d1 > d2 {
				d1, d2 = d2, d1
			}
			if d3 > d4 {
				d3, d4 = d4, d3
			}
			lo := d1
			if d3 > lo {
				lo = d3
			}
			if d2 < d4 {
				d4 = d2
			}
			med := (lo + d4) / 2
			dv := dev(x)
			var r float64 = math.MaxInt32
			if med > 0 {
				r = float64(dv) / float64(med)
			} else if dv == 0 {
				r = 0
			}
			*acc = append(*acc, r)
			x1, x2, x3, x4 = x2, x3, x4, x
		}
	}

	pc := func(v []float64, q float64) float64 {
		if len(v) == 0 {
			return 0
		}
		s := append([]float64{}, v...)
		sort.Float64s(s)
		return s[int(q*float64(len(s)-1))]
	}

	type row struct {
		name string
		wav  string
		db   float64
		mad  float64
		sp   float64
	}
	rows := []row{
		{"room: noise 2 +90sp", "", 0, 2, 90},
		{"room: noise 5 +40sp", "", 0, 5, 40},
		{"bench R idle", "", 0, 5, 40},
	}
	for _, db := range []float64{0, 12, 24, 48} {
		rows = append(rows, row{
			name: "music -" + itoa(int(db)), wav: "/tmp/psync/music16.wav", db: db,
		})
	}
	rows = append(rows,
		row{"music -0 + noise 5 +40sp", "/tmp/psync/music16.wav", 0, 5, 40},
		row{"music -24 + noise 5 +40sp", "/tmp/psync/music16.wav", 24, 5, 40},
		row{"music -48 + noise 5 +40sp", "/tmp/psync/music16.wav", 48, 5, 40},
	)

	t.Logf("%-28s %7s %7s %7s %7s %7s %8s", "input", "p50", "p99", "p99.9", "p99.99", "max", ">10 %")
	for _, tc := range rows {
		if tc.wav != "" {
			if _, err := os.Stat(tc.wav); err != nil {
				t.Logf("%-28s (no fixture)", tc.name)
				continue
			}
		}
		src := []uint16(nil)
		if tc.wav != "" {
			src = loadWav16(t, tc.wav)
			if len(src) < frames*Window {
				continue
			}
		}
		atten := math.Pow(2, tc.db/6)
		s := uint32(0x51ed270b)
		next := func() float64 {
			s ^= s << 13
			s ^= s >> 17
			s ^= s << 5
			return (float64(s) + 0.5) / 4294967296
		}
		var acc []float64
		win := make([]uint16, Window)
		for f := range frames {
			for i := range win {
				y := 0.0
				if src != nil {
					y = float64(int32(src[f*Window+i])-32768) / atten
				}
				if tc.mad > 0 {
					y += tc.mad * 1.65 * (next() + next() + next() - 1.5)
				}
				if tc.sp > 0 && next() < 0.03 {
					if next() < 0.5 {
						y -= tc.sp
					} else {
						y += tc.sp
					}
				}
				y += benchDC
				if y < 0 {
					y = 0
				} else if y > 65535 {
					y = 65535
				}
				win[i] = uint16(y)
			}
			ratios(win, &acc)
		}
		over := 0
		for _, r := range acc {
			if r > 10 {
				over++
			}
		}
		t.Logf("%-28s %7.1f %7.1f %7.1f %7.1f %7.0f %7.3f%%", tc.name,
			pc(acc, .5), pc(acc, .99), pc(acc, .999), pc(acc, .9999),
			pc(acc, 1.0), 100*float64(over)/float64(len(acc)))
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	out := ""
	for v > 0 {
		out = string(rune('0'+v%10)) + out
		v /= 10
	}
	return out
}
