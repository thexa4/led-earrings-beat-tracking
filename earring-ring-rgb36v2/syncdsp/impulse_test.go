package syncdsp

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

// TestMedian5 checks feed's comparator network over every assignment of 0..4 to
// the five inputs, duplicates included: a network wrong for one ordering is
// wrong silently, and its output is a plausible-looking sample.
func TestMedian5(t *testing.T) {
	var v [5]int32
	for n := 0; n < 5*5*5*5*5; n++ {
		k := n
		for i := range v {
			v[i] = int32(k % 5)
			k /= 5
		}
		s := v
		sort.Slice(s[:], func(i, j int) bool { return s[i] < s[j] })
		if got := median5(v[0], v[1], v[2], v[3], v[4]); got != s[2] {
			t.Fatalf("median5%v = %d, want %d", v, got, s[2])
		}
	}
}

// TestImpulseFilter checks feed's impulse filter on the two populations it has to
// separate: tones across the band at room level and four times it must pass
// untouched, and the measured impulses - the LED coupling over music, and the
// dark-bench 40-90 code impulses over a 2-code floor - must be replaced.
func TestImpulseFilter(t *testing.T) {
	const n = 64000
	run := func(gen func(i int) int32) (frac float64) {
		var p Pipeline
		p.Init()
		for i := 0; i < n; i++ {
			p.feed(uint16(1<<15 + gen(i)))
		}
		return float64(p.spCount) / n
	}
	rng := rand.New(rand.NewSource(1))
	noise := func() int32 { return int32(rng.NormFloat64() * 2) }
	tone := func(f, amp float64) func(int) int32 {
		return func(i int) int32 {
			return int32(amp*math.Sin(2*math.Pi*f*float64(i)/SampleRate)) + noise()
		}
	}

	for _, f := range []float64{60, 250, 1000, 3000, 6000} {
		for _, amp := range []float64{270, 1080} {
			if got := run(tone(f, amp)); got > 0.002 {
				t.Errorf("%.0f Hz at %.0f codes: %.2f%% of samples replaced, want ~0",
					f, amp, 100*got)
			}
		}
	}

	var hits int
	impulses := func(base func(int) int32, size func() int32) func(int) int32 {
		return func(i int) int32 {
			v := base(i)
			if rng.Intn(100) < 3 {
				hits++
				v += size()
			}
			return v
		}
	}
	cases := []struct {
		name string
		gen  func(int) int32
	}{
		{"LED coupling over 250 Hz music", impulses(tone(250, 270),
			func() int32 { return 2000 + rng.Int31n(14000) })},
		{"dark bench floor", impulses(func(int) int32 { return noise() },
			func() int32 { return 40 + rng.Int31n(50) })},
	}
	for _, c := range cases {
		hits = 0
		got := run(c.gen)
		want := float64(hits) / n
		if got < 0.9*want || got > 1.1*want {
			t.Errorf("%s: replaced %.2f%% of samples, %.2f%% were impulses",
				c.name, 100*got, 100*want)
		}
	}
}
