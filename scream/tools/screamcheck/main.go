// Command screamcheck exercises the "scream" audio transport with the real
// encoder/decoder from codeberg.org/maaike328p/datatrans (the same code the
// browser wasm and the earring firmware use).
//
//	screamcheck wav  <file> [seconds] > out.wav   # make audio, like the 📢 button
//	screamcheck time <file> [trials] [noise] [layout]  # how long a transfer takes
//
// layout is device (default, the firmware's buffers), overlap (the firmware
// before the fountain was bounded) or separate (no window overlay).
//
// Build (the module is usually already in the local module cache):
//
//	GOFLAGS=-mod=mod GOPROXY=off go build -o screamcheck .
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"unsafe"

	"codeberg.org/maaike328p/datatrans"
)

// The earring samples the microphone at 10 kHz, which means 200 samples per
// 20 ms symbol. The browser encodes at its AudioContext rate (44.1 or 48 kHz);
// any rate that is a multiple of 50 gives identical framing, so 10 kHz is used
// here to match the receiver.
const sampleRate = 10000

// Receiver-side limits, see earring-ring-rgb36v2/datarecv.go.
const (
	maxProgramSize = 1532
	bufSize        = maxProgramSize + 1024
)

// layout selects how the receive buffers are laid out in memory.
type layout int

const (
	// layoutDevice is the firmware's: one animationBuf of bufSize/4 words,
	// word 0 reserved for the size/hash header, the 20 ms window
	// (samplesPerSymbol+2 words) overlaid on its tail, and the fountain
	// given only the words in between.
	layoutDevice layout = iota
	// layoutOverlap is the firmware before the fountain was bounded: the
	// same overlay, but the fountain gets animationBuf[1:] and so shares
	// its top slots with the window.
	layoutOverlap
	// layoutSeparate gives the fountain all of animationBuf[1:] and the
	// window its own buffer (no overlay at all).
	layoutSeparate
)

var layoutNames = map[string]layout{
	"device": layoutDevice, "overlap": layoutOverlap, "separate": layoutSeparate,
}

// receiveBuffers returns the fountain storage and the decoder window for l.
func receiveBuffers(l layout, samplesPerSymbol int) (fountain []uint32, window []int32) {
	buf := make([]uint32, bufSize/4)
	windowWords := samplesPerSymbol + 2
	if l == layoutSeparate {
		return buf[1:], make([]int32, windowWords)
	}
	tail := len(buf) - windowWords
	window = unsafe.Slice((*int32)(unsafe.Pointer(&buf[tail])), windowWords)
	if l == layoutOverlap {
		return buf[1:], window
	}
	return buf[1:tail], window
}

func encodeWav(samples []float32, rate int) []byte {
	out := make([]byte, 44+len(samples)*2)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(samples)*2))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:24], 1) // mono
	binary.LittleEndian.PutUint32(out[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(rate*2))
	binary.LittleEndian.PutUint16(out[32:34], 2)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(samples)*2))
	for i, s := range samples {
		v := int32(math.Round(float64(s) * 32767))
		v = max(-32768, min(32767, v))
		binary.LittleEndian.PutUint16(out[44+i*2:], uint16(v))
	}
	return out
}

func cmdWav(args []string) {
	data, err := os.ReadFile(args[0])
	if err != nil {
		panic(err)
	}
	rate := 48000
	seconds := 8
	if len(args) > 1 {
		seconds, _ = strconv.Atoi(args[1])
	}
	e := datatrans.NewEncoder(rate)
	e.Start(data, 12345)
	buf := make([]float32, rate*seconds)
	e.Encode(buf)

	peak, sum := float32(0), 0.0
	for _, s := range buf {
		peak = max(peak, float32(math.Abs(float64(s))))
		sum += float64(s) * float64(s)
	}
	fmt.Fprintf(os.Stderr, "peak %.3f (%.1f dBFS), rms %.4f (%.1f dBFS)\n",
		peak, 20*math.Log10(float64(peak)),
		math.Sqrt(sum/float64(len(buf))),
		20*math.Log10(math.Sqrt(sum/float64(len(buf)))+1e-12))
	os.Stdout.Write(encodeWav(buf, rate))
}

// transfer runs one encoder -> decoder round trip, like the earring would do:
// the receiver starts at a random point in the symbol stream and keeps feeding
// 20 ms windows to the decoder. noise is the stddev of added Gaussian noise, in
// full-scale units (a single tone has an amplitude of 1/32 = 0.031).
func transfer(data []byte, noise float64, r *rand.Rand, l layout) (symbols int, ok bool) {
	samplesPerSymbol := sampleRate / datatrans.SymbolsPerSecond

	recv, window := receiveBuffers(l, samplesPerSymbol)
	var decoder datatrans.Decoder
	decoder.Initialize(recv, maxProgramSize, sampleRate)

	encoder := datatrans.NewEncoder(sampleRate)
	encoder.Start(data, r.Uint32())

	send := make([]float32, samplesPerSymbol)
	// Sentinel telling the Goertzel loop where the window ends.
	window[samplesPerSymbol] = 0x7fff_ffff
	window[samplesPerSymbol+1] = 0

	// Start partway into a symbol, so the receiver has to lock on.
	encoder.Encode(send[:r.IntN(samplesPerSymbol)])

	for range 100000 {
		encoder.Encode(send)
		for i, s := range send {
			if noise > 0 {
				s += float32(r.NormFloat64() * noise)
			}
			window[i] = int32(s * 1000)
		}
		status := decoder.ProcessSamples(window)
		symbols++
		switch {
		case status > 0:
			// Receiver wants to skip ahead to line up with a symbol boundary.
			encoder.Encode(send[:status])
		case status == datatrans.ErrCodeReceived:
			return symbols, bytes.Equal(data, decoder.Bytes())
		case status < 0:
			// Gave up on this transmission, listen for the next one.
			decoder.Reset()
		}
	}
	return symbols, false
}

func cmdTime(args []string) {
	data, err := os.ReadFile(args[0])
	if err != nil {
		panic(err)
	}
	trials := 100
	if len(args) > 1 {
		trials, _ = strconv.Atoi(args[1])
	}
	noises := []float64{0, 1.0 / 23}
	if len(args) > 2 {
		f, err := strconv.ParseFloat(args[2], 64)
		if err == nil {
			noises = []float64{f}
		}
	}
	l := layoutDevice
	if len(args) > 3 {
		var known bool
		if l, known = layoutNames[args[3]]; !known {
			fmt.Fprintln(os.Stderr, "unknown layout "+args[3])
			os.Exit(1)
		}
	}
	for _, noise := range noises {
		r := rand.New(rand.NewPCG(42, 7))
		var times []float64
		ok := 0
		for range trials {
			symbols, success := transfer(data, noise, r, l)
			if success {
				ok++
				times = append(times, float64(symbols)/datatrans.SymbolsPerSecond)
			}
		}
		if ok == 0 {
			fmt.Printf("noise=%g: 0/%d received\n", noise, trials)
			continue
		}
		sort.Float64s(times)
		mean := 0.0
		for _, t := range times {
			mean += t
		}
		mean /= float64(ok)
		fmt.Printf("noise=%.4f: %d/%d received  median=%.2fs mean=%.2fs "+
			"p90=%.2fs max=%.2fs  %.0f B/s\n",
			noise, ok, trials, times[ok/2], mean, times[ok*9/10], times[ok-1],
			float64(len(data))/mean)
	}
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: screamcheck wav <file> [seconds] | screamcheck time <file> [trials] [noise] [device|overlap|separate]")
		os.Exit(1)
	}
	switch os.Args[1] {
	case "wav":
		cmdWav(os.Args[2:])
	case "time":
		cmdTime(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "unknown command "+os.Args[1])
		os.Exit(1)
	}
}
