package syncdsp

// Layout export for the SWD read-out tools.
//
// tools/specdump.py, tools/level.py and friends read a Pipeline straight out of
// RAM over the debug probe, so they need the byte offset of every field they
// touch. Those offsets used to be hardcoded in the Python and silently went
// stale: a field read at the wrong offset still returns a plausible-looking
// number (this read "agcShift = 0" on a device that was demonstrably running at
// gain 64), which is much worse than an error.
//
// Emit the truth from the compiler instead:
//
//	go test ./syncdsp/ -run TestExportLayout -v
//	WRITE_LAYOUT=1 go test ./syncdsp/ -run TestExportLayout
//
// The offsets are only valid for the exact source tree they were generated
// from, so the file carries a content hash of the struct definition and the
// tools refuse to read a pipeline whose hash does not match the firmware they
// were pointed at.
//
// Safe on the host because every field up to `beat` is int32/uint32/int8-array/
// bool: no int, pointer or slice, so the 32-bit ARM and amd64 layouts agree.
// Fields at or after BeatTracker are deliberately not exported - check there
// first if you add one.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"unsafe"
)

type layoutField struct {
	Offset int `json:"offset"` // byte offset from the start of Pipeline
	Words  int `json:"words"`  // 32-bit words (arrays only; 1 otherwise)
}

// LayoutFile is what tools/layout.json holds.
type LayoutFile struct {
	NBands  int                    `json:"nbands"`
	Window  int                    `json:"window"`
	SrcHash string                 `json:"src_hash"`
	Fields  map[string]layoutField `json:"fields"`
}

// fingerprint of the fields that define the layout, so a stale JSON file cannot
// be used silently.
func fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%d/%d %v %v %v %v %v %v",
		NBands, Window,
		unsafe.Sizeof(bank{}), unsafe.Sizeof(Pipeline{}),
		unsafe.Offsetof(Pipeline{}.smooth), unsafe.Offsetof(Pipeline{}.energies),
		unsafe.Offsetof(Pipeline{}.agcPeak), unsafe.Offsetof(Pipeline{}.silent))
	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

func layout() LayoutFile {
	f := map[string]layoutField{}
	add := func(name string, off uintptr, words int) {
		f[name] = layoutField{Offset: int(off), Words: words}
	}
	add("filters.energy", unsafe.Offsetof(Pipeline{}.filters.energy), NBands)
	add("smooth", unsafe.Offsetof(Pipeline{}.smooth), NBands)
	add("prevSmooth", unsafe.Offsetof(Pipeline{}.prevSmooth), NBands)
	add("smoothSnap", unsafe.Offsetof(Pipeline{}.smoothSnap), NBands)
	add("energies", unsafe.Offsetof(Pipeline{}.energies), NBands)
	add("agcPeak", unsafe.Offsetof(Pipeline{}.agcPeak), 1)
	add("agcShift", unsafe.Offsetof(Pipeline{}.agcShift), 1)
	add("agcHold", unsafe.Offsetof(Pipeline{}.agcHold), 1)
	add("total", unsafe.Offsetof(Pipeline{}.total), 1)
	add("prevTotal", unsafe.Offsetof(Pipeline{}.prevTotal), 1)
	add("fluxFloor", unsafe.Offsetof(Pipeline{}.fluxFloor), 1)
	add("level", unsafe.Offsetof(Pipeline{}.level), 1)
	add("linPeak", unsafe.Offsetof(Pipeline{}.linPeak), 1)
	add("onsetAge", unsafe.Offsetof(Pipeline{}.onsetAge), 1)
	return LayoutFile{
		NBands:  NBands,
		Window:  Window,
		SrcHash: fingerprint(),
		Fields:  f,
	}
}

func TestExportLayout(t *testing.T) {
	l := layout()
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pipeline layout (%s):\n%s", l.SrcHash, b)
	if os.Getenv("WRITE_LAYOUT") != "" {
		if err := os.WriteFile("layout.json", b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote syncdsp/layout.json")
	}
}
