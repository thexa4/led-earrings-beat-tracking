// Command hosttrace replays a PCM16 mono WAV through the exact device
// pipeline and dumps the per-frame telemetry records as CSV, same columns
// as tools/telem.py produces for a device run (minus t/mark, plus frame).
//
//	go run ./tools/hosttrace /tmp/psync/seg300.wav > ideal300.csv
//
// Because syncdsp.recordTelemetry runs in endFrame on the host too, this is
// the *ideal* trace: what a perfectly-placed, noiseless earring would decide
// for the same music. Device runs are aligned to it (hash + total) and then
// compared to it and to each other in music time.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"

	"github.com/aykevl/things/earring-ring-rgb36v2/syncdsp"
)

// readWav returns the samples of a PCM16 mono WAV file (copy of hostbeat's).
func readWav(path string) []uint16 {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		panic("not a RIFF/WAVE file")
	}
	data := []byte(nil)
	for i := 12; i+8 <= len(b); {
		id, n := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:i+8]))
		if id == "data" {
			if i+8+n > len(b) {
				n = len(b) - 8
			}
			data = b[i+8 : i+8+n]
		}
		i += 8 + n + n%2
	}
	out := make([]uint16, len(data)/2)
	for i := range out {
		// The pipeline expects ADC-style unsigned offset-binary (it subtracts
		// 1<<15), so bias the two's-complement WAV sample. Passing it raw
		// makes the pipeline see the signal minus a ±16384 square wave locked
		// to every zero crossing, which the 30 Hz DC blocker passes straight
		// through: every ideal trace measured that square wave, not the music.
		out[i] = uint16(int32(int16(binary.LittleEndian.Uint16(data[2*i:]))) + 32768)
	}
	return out
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hosttrace SEG.WAV > ideal.csv")
		os.Exit(1)
	}
	samples := readWav(os.Args[1])

	var p syncdsp.Pipeline
	p.Init()

	w := os.Stdout
	fmt.Fprintln(w, "frame,hash,onset,beat,bar,anchor,pchange,locked,silent,pushed,pattern,votes,period,total,phase,onset_age,settled,primed,nov,d2,d4,d8")
	f := 0
	for i := 0; i+syncdsp.Window <= len(samples); i += syncdsp.Window {
		for _, v := range samples[i : i+syncdsp.Window] {
			p.ProcessSample(v)
		}
		p.EndFrame()

		// Newest record: slot (Seq-1) % TelemRecords.
		t := &syncdsp.Telem
		if t.Seq == 0 {
			continue
		}
		base := int((t.Seq-1)%uint32(syncdsp.TelemRecords)) * 4
		r := t.Records[base : base+4]
		flags := r[1]
		b := func(bit uint32) string {
			if flags&(1<<bit) != 0 {
				return "True"
			}
			return "False"
		}
		cols := []string{
			fmt.Sprint(f),
			fmt.Sprintf("%08x", r[0]),
			b(0), b(1), b(2), b(3), b(4), b(5), b(6), b(7),
			fmt.Sprint((flags >> 8) & 7),
			fmt.Sprint((flags >> 16) & 0xFF),
			fmt.Sprint((flags >> 24) & 0x7F),
			fmt.Sprint(r[2] & 0xFFFF),
			fmt.Sprint((r[2] >> 16) & 0x7F),
			fmt.Sprint((r[2] >> 23) & 0x7F),
			b(30), b(31),
			fmt.Sprint(r[3] & 0xFF),
			fmt.Sprint((r[3] >> 8) & 0xFF),
			fmt.Sprint((r[3] >> 16) & 0xFF),
			fmt.Sprint((r[3] >> 24) & 0xFF),
		}
		fmt.Fprintln(w, strings.Join(cols, ","))
		f++
	}
}
