package syncdsp

// Replay a WAV file through the pipeline with full internal visibility.
// Runs only when HOSTBEAT points at a PCM16 mono WAV:
//
//	HOSTBEAT=/tmp/seg120.wav go test -run TestReplayWav ./syncdsp/
//
// Prints the interval histogram and lock trace - the internal view the
// device telemetry only exposes 8 bits of.

import (
	"encoding/binary"
	"fmt"
	"os"
	"testing"
)

func TestReplayWav(t *testing.T) {
	path := os.Getenv("HOSTBEAT")
	if path == "" {
		t.Skip("HOSTBEAT not set")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rate, data := 0, []byte(nil)
	for i := 12; i+8 <= len(b); {
		id, n := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:i+8]))
		if id == "fmt " {
			rate = int(binary.LittleEndian.Uint32(b[i+12 : i+16]))
		}
		if id == "data" {
			data = b[i+8 : i+8+n]
		}
		i += 8 + n + n%2
	}
	fps := float64(rate) / Window

	var p Pipeline
	p.Init()
	var prevOnsets int
	f := 0
	for ; f*Window+Window <= len(data)/2; f++ {
		for i := f * Window; i < (f+1)*Window; i++ {
			p.ProcessSample(binary.LittleEndian.Uint16(data[2*i:]))
		}
		ev := p.EndFrame()
		if ev.Onset && ev.Locked {
			// After lock, show phase error vs onset: is the PLL tracking?
			fmt.Printf("t=%5.1f onset phase=%d period=%d\n",
				float64(f)/fps, p.beat.phase, p.beat.Period)
		}
		if ev.Onset {
			prevOnsets++
		}
	}
	fmt.Printf("frames=%d fps=%.1f onsets=%d locked=%v period=%d\n",
		f, fps, prevOnsets, p.beat.Locked, p.beat.Period)
	fmt.Print("ivis:")
	for i, c := range p.beat.ivis {
		if c > 0 {
			fmt.Printf(" %d:%d", i+BeatMinPeriod, c)
		}
	}
	fmt.Println()
}
