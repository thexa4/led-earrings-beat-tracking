//go:build !micdump

package main

// Stub half of the microphone waveform recorder; sync_micdump.go (built with
// -tags micdump) holds the real thing, including why it exists.
//
// Not built by default for two reasons, and the second is the one to remember.
// The recorder costs more flash than the image has. And it answers the wrong
// question: it stores 8 bits per sample, divided by the AGC's own peak, so the
// waveform is a coarse envelope with the carrier removed. Measured on a pair of
// earrings hearing the same music through one speaker, the two 10-second
// recordings correlate at 0.03 after band-limiting, and hold only 18-20 distinct
// sample values each, where two microphones in one room should correlate near 1.
// Anything derived from them - cross-correlation, lag, spectral comparison - is
// reading the quantiser, not the sound.
//
// For the questions that matter there are better probes that need no firmware
// support at all: the telemetry ring gives the decisions (tools/telem.py,
// tools/paircapture.py), and tools/spikestat.py, tools/clip.py and
// tools/freeze.py read adcData at full 16-bit resolution with TIM22 stopped, so
// the window they measure is one coherent capture of the pipeline's actual input.
const micDumpEnabled = false

func micDumpAdd(uint16) {}

func micDumpUpdateGain() {}
