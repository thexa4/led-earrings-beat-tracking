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
	"strings"
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
	rows := []string(nil)
	f := 0
	for ; f*Window+Window <= len(data)/2; f++ {
		for i := f * Window; i < (f+1)*Window; i++ {
			p.ProcessSample(binary.LittleEndian.Uint16(data[2*i:]))
		}
		ev := p.EndFrame()
		rows = append(rows, fmt.Sprintf("%d,%v,%v,%v,%v,%v,%d,%d,%08x,%v,%d,%d",
			f, ev.Onset, p.bassOnset, ev.Beat, ev.Bar, ev.Locked, p.beat.Period,
			p.total, ev.Hash, ev.Anchor, p.smooth[NBands-1],
			p.env[(p.envHead-1)&EnvMask]))
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
	if out := os.Getenv("REPLAY_CSV"); out != "" {
		csv := "frame,onset,bass,beat,bar,locked,period,total,hash,anchor,basslog,env\n" +
			strings.Join(rows, "\n") + "\n"
		if err := os.WriteFile(out, []byte(csv), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Top candidate periods by accumulated envelope-autocorrelation evidence
	// (tempoScan), so the winner and its margin over the runner-up are visible.
	type sc struct {
		p int32
		v int32
	}
	var top [3]sc
	for i, e := range p.beat.ev {
		for j := 0; j < 3; j++ {
			if int32(e) > top[j].v {
				for k := 2; k > j; k-- {
					top[k] = top[k-1]
				}
				top[j] = sc{int32(i) + BeatMinPeriod, int32(e)}
				break
			}
		}
	}
	fmt.Printf("ev:")
	for _, t := range top {
		if t.v != 0 {
			fmt.Printf(" %d:%d", t.p, t.v)
		}
	}
	fmt.Println()
}
