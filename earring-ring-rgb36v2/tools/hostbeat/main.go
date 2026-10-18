// Command hostbeat replays a PCM WAV file through the exact device pipeline
// and prints the beat-tracking trace, so DSP behaviour can be checked at any
// sample rate without a device (the device runs at 8.8 kSPS; the bank
// constants assume 16 kSPS - this tool exists to compare the two).
//
//	go run ./tools/hostbeat /tmp/seg.wav [fs]   (fs only labels the output)
package main

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/aykevl/things/earring-ring-rgb36v2/syncdsp"
)

// readWav returns the samples of a PCM16 mono WAV file.
func readWav(path string) ([]uint16, int) {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		panic("not a RIFF/WAVE file")
	}
	rate, data := 0, []byte(nil)
	for i := 12; i+8 <= len(b); {
		id, n := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:i+8]))
		switch id {
		case "fmt ":
			rate = int(binary.LittleEndian.Uint32(b[i+12 : i+16]))
			if ch := binary.LittleEndian.Uint16(b[i+10 : i+12]); ch != 1 {
				panic("need mono")
			}
		case "data":
			if i+8+n > len(b) {
				n = len(b) - i - 8
			}
			data = b[i+8 : i+8+n]
		}
		i += 8 + n + n%2
	}
	out := make([]uint16, len(data)/2)
	for i := range out {
		out[i] = binary.LittleEndian.Uint16(data[2*i:])
	}
	return out, rate
}

func main() {
	samples, rate := readWav(os.Args[1])
	if len(os.Args) > 2 {
		fmt.Sscanf(os.Args[2], "%d", &rate)
	}
	fs := rate

	var p syncdsp.Pipeline
	p.Init()

	beatFrames := []int32{}
	var onsets int
	lockFrame, lockPeriod := -1, int32(0)
	f := 0
	for i := 0; i+syncdsp.Window <= len(samples); i += syncdsp.Window {
		for _, v := range samples[i : i+syncdsp.Window] {
			p.ProcessSample(v)
		}
		ev := p.EndFrame()
		if ev.Onset {
			onsets++
		}
		if ev.Beat {
			beatFrames = append(beatFrames, int32(f))
		}
		if ev.Locked && lockFrame < 0 {
			lockFrame, lockPeriod = f, p.BeatPeriod()
		}
		f++
	}
	fmt.Printf("onsets: %d (%.1f/s), lock at frame %d",
		onsets, float64(onsets)/float64(f)*float64(rate)/syncdsp.Window, lockFrame)
	if lockFrame >= 0 {
		fmt.Printf(", period %d frames = %.1f BPM", lockPeriod,
			60*float64(rate)/(float64(lockPeriod)*syncdsp.Window))
	}
	fmt.Println()

	// Collapse runs of consecutive flagged frames into beat events, then
	// report the spacing histogram: the ground truth for "what tempo did the
	// pipeline hear".
	fmt.Printf("%d frames at %d fps (fs=%d)\n", f, rate/syncdsp.Window, fs)
	fmt.Printf("beat-flag frames: %d\n", len(beatFrames))
	if len(beatFrames) == 0 {
		return
	}
	ev := []int32{beatFrames[0]}
	for i, b := range beatFrames {
		if i > 0 && b-ev[len(ev)-1] > 2 {
			ev = append(ev, b)
		}
	}
	hist := map[int32]int{}
	for i := 1; i < len(ev); i++ {
		hist[ev[i]-ev[i-1]]++
	}
	fmt.Printf("beat events: %d, spacing (frames): %v\n", len(ev), hist)
	for d, n := range hist {
		if n >= 3 {
			fmt.Printf("  %d frames -> %.1f BPM\n", d, 60*float64(rate)/(float64(d)*syncdsp.Window))
		}
	}
}
