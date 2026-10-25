//go:build micdump

package main

// The microphone waveform recorder, compiled only with -tags micdump.
//
// It is out of the default build because it no longer fits: this image is a few
// hundred bytes over FLASH_TEXT and the recorder is the largest thing in it that
// nothing reads. It is also the wrong instrument - see the note in
// sync_micdump_off.go about why an 8-bit, AGC-ranged capture cannot answer
// questions about the waveform. Build it back with:
//
//	tinygo build -tags micdump -o firmware.hex -target=./stm32l0x1.json -opt=2
//
// and expect to free flash somewhere else first. tools/micdump.py reads it.
// The mic dump: a byte-wide capture of the microphone stream for a debugger,
// overlaid on animationBuf - which sync mode never touches otherwise, since it
// is the fountain decoder's scratch buffer (its own overlay, adcDataNorm, is
// written by dataRecv alone).
//
// Why a separate buffer instead of reading adcData over SWD: that ring holds
// 250 samples (15.6 ms) while reading it takes ~100 ms, so the DMA rewrites it
// while it is being read and the reconstruction comes back torn beyond
// recognition (measured: no music survives). Trading resolution for time fixes
// that, and the wire forces the same trade anyway - OpenOCD sustains ~17 kB/s
// of mdw output regardless of adapter clock (measured at 950 kHz and 4 MHz),
// which is less than 16-bit at 16 kSPS needs. 8-bit samples at 8 kSPS (2-tap
// average, so 8-16 kHz does not just alias down) take half the wire and leave
// room for the other earring's stream on the same host.
//
// Layout, from the base of animationBuf: micDumpSize sample bytes, then two
// control words (samples written, DC estimate). The counter sits *above* the
// ring so that one ascending mdw read of the whole region samples it last: the
// value then bounds everything the ring can contain, which makes a window read
// race-free with no freezing of the CPU (see tools/micdump.py).
const (
	// Capture ring, in samples (= bytes). Power of two so the wrap is a mask.
	micDumpSize = 2048 // 256 ms at 8 kSPS

	// micDumpDecim divides the 16 kSPS sample stream down to the dump rate.
	micDumpDecim = 2

	// The dump's scale: the deviation is divided by 2^shift, and that shift is
	// picked from the AGC's input peak so all eight bits of the byte are used
	// whatever the room is doing. micDumpTarget is where the tracked peak lands;
	// micDumpShiftMax stops the shift running away when the input is dead
	// silent, where it would otherwise amplify the ADC's own noise.
	micDumpTarget   = 100 // aim the tracked peak at ~+-100 codes
	micDumpShiftMax = 12

	// DC filter time constant, as a shift: 1024 dump samples = 128 ms.
	micDumpDCShift = 10

	// micDumpDCShift and the deviation are both kept with micDumpDCFrac extra
	// fraction bits. Without them the filter cannot work at all: a truncating
	// `(s - dc) >> 10` returns 0 for |s - dc| < 1024, so once the deviation
	// settles below that - which it always does, the room peak here is a few
	// hundred codes - the tracker freezes wherever it happened to stop and the
	// ring records a constant. Measured: a stuck +31 in every byte, which read
	// as "the microphone captures nothing".
	micDumpDCFrac = 4
)

var _ [len(animationBuf)*4 - micDumpSize - 12]byte // ring + control words fit

// Control words live inside animationBuf too: this build has ~250 bytes of RAM
// free, which is less than anything worth recording would need.
//
// The write counter must stay the *highest* of them: tools/micdump.py reads the
// whole region with one ascending mdw, so the counter it sees is the last thing
// sampled and therefore bounds everything below it.
var (
	// Signed view of the tracked DC level, in Q micDumpDCFrac ADC codes.
	micDumpDC = (*int32)(unsafe.Pointer(&animationBuf[micDumpSize/4]))
	// Applied scale, as the shift used on the deviation (see micDumpShiftMax).
	micDumpGain = &animationBuf[micDumpSize/4+1]
	micDumpW    = &animationBuf[micDumpSize/4+2] // samples stored since boot

	micDumpPrev  uint16 // first tap of the 2:1 decimator
	micDumpPhase uint8  // taps collected so far
)

func micDumpRing() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(&animationBuf[0])), micDumpSize)
}

// micDumpAdd records one sample: averaged with its predecessor, centred on a
// slowly tracked DC level, and scaled into a signed byte.
//
// It is deliberately out of line. The store goes through an unsafe.Pointer into
// animationBuf, and a call boundary already forces LLVM to assume memory
// changed - so the caller's register allocation survives the aliasing, whereas
// inlining this into the sample loop cost ~600 bytes of flash when the same
// trick was tried for the sync pipeline.
//
//go:noinline
func micDumpAdd(sample uint16) {
	if micDumpPhase == 0 {
		micDumpPrev = sample
		micDumpPhase = 1
		return
	}
	micDumpPhase = 0
	s := (int32(sample) + int32(micDumpPrev)) >> 1
	dev := (s << micDumpDCFrac) - *micDumpDC
	if !micDumpSeeded {
		// Start centred, instead of clipping for the first second.
		*micDumpDC = s << micDumpDCFrac
		micDumpSeeded = true
	}
	*micDumpDC += (dev + 1<<(micDumpDCShift-1)) >> micDumpDCShift

	// Scale: aim the AGC's tracked input peak at roughly the middle of the
	// signed byte. *micDumpGain is refreshed once per analysis frame by
	// syncDumpUpdateGain - doing this per sample would put a pipeline call in
	// the hot loop for a value that moves on an ~8 s time constant.
	sh := int32(*micDumpGain)
	v := (dev + 1<<(micDumpDCFrac+int(sh)-1)) >> (micDumpDCFrac + int(sh))
	switch {
	case v > 127:
		v = 127
	case v < -128:
		v = -128
	}
	w := *micDumpW
	micDumpRing()[w&(micDumpSize-1)] = byte(v)
	*micDumpW = w + 1
}

// micDumpSeeded marks "the DC estimate has a first sample in it". It cannot be
// inferred from the DC word itself, since a legitimate estimate is rarely zero
// but can be, nor from the counter, which is written after the sample.
var micDumpSeeded bool

// micDumpUpdateGain picks the dump's scale from the pipeline's input peak.
// Called at the analysis-frame boundary, never per sample: the value it reads
// moves on the AGC's ~8 s peak-hold time constant, so running it more often
// would only add a call to the sample loop.
//
// Deliberately a slow follower, and deliberately shared with the DSP: a dump
// that ranged per sample would chase the music itself and turn the recording
// into an envelope-followed mess that no longer resembles what was heard.
func micDumpUpdateGain() {
	pk := syncPipe.InputPeak()
	sh := int32(0)
	for p := pk >> 1; p > micDumpTarget && sh < micDumpShiftMax; p >>= 1 {
		sh++
	}
	*micDumpGain = uint32(sh)
}
