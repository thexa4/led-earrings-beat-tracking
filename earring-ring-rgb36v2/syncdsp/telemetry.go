package syncdsp

// Debug-branch telemetry: a fixed-layout ring of per-frame records plus a
// control block, polled by the host over SWD (OpenOCD telnet) while the
// pipeline keeps running. Nothing here halts or perturbs the pipeline beyond
// a handful of stores per frame boundary.
//
// The host resolves the block address from firmware.elf (readelf -sW), so
// syncdsp.Telem plus the field layout below are the contract with
// tools/telem.py. All words little-endian (Cortex-M0+). Tunables are not
// poked (they constant-fold; see consts.go): experiments change them by
// rebuild+flash via tools/exp.sh.
//
// RAM budget: this branch trades the second custom pattern slot (1,536 B of
// flash) for code; RAM was the tight side (1,012 B free on master), hence
// 4-word records and no frame counter word (the host gets frame numbers from
// Seq).

const (
	// TelemMagic lets the host notice firmware that predates (or moved) the
	// telemetry block instead of decoding garbage.
	TelemMagic = 0x544C4D31 // "TLM1"

	// 40 records * 4 words = 640 B. At 64 analysis frames/s the host must
	// poll at least once per 0.6 s to never lose a frame; Seq gaps make any
	// loss explicit, and the sticky event fields survive overwrite, so the
	// event history is never lost, only the spacing between lost frames.
	// tools/telem.py mirrors this as RECORDS.
	TelemRecords = 40
	telemWords   = 4
)

// Telemetry control block + ring. Written by the device, read (and partly
// written: HostMark) by the host.
type Telemetry struct {
	Magic       uint32
	Seq         uint32 // frames recorded since Init (== pipeline frame)
	Head        uint32 // ring slot of the next record
	HostMark    uint32 // host writes non-zero to stamp the next record
	LastMark    uint32 // host-visible acknowledgement of HostMark
	LastAnchor  uint32 // Seq of the most recent anchor
	LastPattern uint32 // Seq of last pattern change | pattern << 16
	LastLock    uint32 // Seq of most recent unlock->lock transition
	LastUnlock  uint32 // Seq of most recent lock->unlock transition
	Records     [TelemRecords * telemWords]uint32
}

// Telem is the block the host talks to. Package-level so it has a stable ELF
// symbol (syncdsp.Telem).
var Telem Telemetry

// Record layout (4 words):
//
//	w0  hash
//	w1  flags (below)
//	w2  total (log2 frame loudness, Q8, low 16 bits) | phase<<16 (7 bits) |
//	    onsetAge<<23 (7 bits, saturated at 100) | settled<<30 | primed<<31
//	w3  novelty | lag2<<8 | lag4<<16 | lag8<<24 (0xFF = not computed/absent;
//	    computed on ring-push and anchor frames only)
//
// Flag bits:
//
//	0 onset   1 beat   2 bar   3 anchor   4 pattern-changed
//	5 locked  6 silent 7 pushed (ring written this frame)  11 host-mark
//	12 returned (periodicity confirmation)  13 modulating (see ModEnterQ8)
//	8..10 pattern   16..23 lock votes (clamped)   24..30 period, frames
const (
	flagOnset   = 1 << 0
	flagBeat    = 1 << 1
	flagBar     = 1 << 2
	flagAnchor  = 1 << 3
	flagPChange = 1 << 4
	flagLocked  = 1 << 5
	flagSilent  = 1 << 6
	flagPushed  = 1 << 7
	flagMark    = 1 << 11
	flagReturn  = 1 << 12 // periodicity confirmation (Events.Returned)
	flagMod     = 1 << 13 // modulation-depth gate open: something is playing
	// notApplicable marks distance fields that were not computed this frame.
	notApplicable = 0xFF
)

var telemWasLocked bool // for lock/unlock transition seqs

// recordTelemetry appends one record for the frame that just ended. Called
// from endFrame, so host runs of the pipeline record too: the same decoder
// (tools/telem.py) reads device traces and ideal host traces alike.
//
//go:noinline
func (p *Pipeline) recordTelemetry() {
	t := &Telem
	if t.Magic != TelemMagic {
		*t = Telemetry{Magic: TelemMagic}
	}
	seq := t.Seq
	r := t.Records[t.Head*telemWords : t.Head*telemWords+telemWords]

	ev := &p.events
	var flags uint32
	if ev.Onset {
		flags |= flagOnset
	}
	if ev.Beat {
		flags |= flagBeat
	}
	if ev.Bar {
		flags |= flagBar
	}
	if ev.Anchor {
		flags |= flagAnchor
	}
	if ev.PatternChanged {
		flags |= flagPChange
	}
	if ev.Locked {
		flags |= flagLocked
	}
	if ev.Silent {
		flags |= flagSilent
	}
	if ev.HashAge == 0 {
		flags |= flagPushed
	}
	if ev.Returned {
		flags |= flagReturn
	}
	if ev.Modulating {
		flags |= flagMod
	}
	if t.HostMark != 0 {
		t.LastMark = t.HostMark
		t.HostMark = 0
		flags |= flagMark
	}
	flags |= uint32(ev.Pattern&7) << 8
	// bit 16..23 used to carry the interval-histogram vote count; the histogram
	// is gone (see tempoScan) and the field read a constant zero, so nothing is
	// written here and the tools ignore those bits.
	period := p.beat.Period
	if period < 0 {
		period = 0
	}
	flags |= uint32(period&0x7f) << 24

	// Distance fields: novelty and the three lag minima, computed only on
	// frames where a decision could consume them (a ring push, or an anchor
	// frame): ~20 extra hashDists on those few frames per second is nothing
	// on the M0+, and it is exactly the margin data the tuner needs.
	var nov, d2, d4, d8 uint32 = notApplicable, notApplicable, notApplicable, notApplicable
	if ev.HashAge == 0 || ev.Anchor {
		nov = uint32(p.noveltyMin(ev.Hash))
		if ev.Locked {
			d2, d4, d8 = p.lagDists(ev.Hash)
		}
	}

	onsetAge := p.onsetAge
	if onsetAge > 100 {
		onsetAge = 100
	}
	r[0] = ev.Hash
	r[1] = flags
	r[2] = uint32(p.total&0xffff) | uint32(p.beat.phase&0x7f)<<16 |
		uint32(onsetAge)<<23 | boolBit(p.stableFrames >= AnchorSettleFrames, 30) |
		boolBit(p.ringCount >= 4, 31)
	r[3] = nov | d2<<8 | d4<<16 | d8<<24

	// Sticky history (survives ring overwrite).
	if ev.Anchor {
		t.LastAnchor = seq
	}
	if ev.PatternChanged {
		t.LastPattern = seq | uint32(ev.Pattern)<<16
	}
	if ev.Locked && !telemWasLocked {
		t.LastLock = seq
	} else if !ev.Locked && telemWasLocked {
		t.LastUnlock = seq
	}
	telemWasLocked = ev.Locked

	t.Seq = seq + 1
	t.Head = (t.Head + 1) % TelemRecords
}

func boolBit(v bool, shift uint32) uint32 {
	if v {
		return 1 << shift
	}
	return 0
}

// lagDists returns the minimum hash distance to the ring at each nominal lag
// (2, 4, 8 bars, +-LagTolerance beats), or 0xFF where no entry exists.
// Telemetry only: decide() keeps its own early-exit matching.
//
//go:noinline
func (p *Pipeline) lagDists(h uint32) (uint32, uint32, uint32) {
	out := [3]uint32{notApplicable, notApplicable, notApplicable}
	for i, lagBars := range [3]int32{LagBars1, LagBars2, LagBars3} {
		nominal := lagBars * BeatsPerBar
		for off := -LagTolerance; off <= LagTolerance; off++ {
			lag := nominal + int32(off)
			if lag < 1 || p.beat.beat < lag || p.ringCount <= lag {
				continue
			}
			prev := p.ring[(p.beat.beat-lag)&(RingSize-1)]
			if prev == 0 {
				continue
			}
			d := uint32(ringDist(h, prev))
			if d < out[i] {
				out[i] = d
			}
		}
	}
	return out[0], out[1], out[2]
}
