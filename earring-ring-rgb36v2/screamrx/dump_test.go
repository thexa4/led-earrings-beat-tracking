package screamrx

import (
	"encoding/binary"
	"os"
	"testing"
)

// TestDumpStream writes the symbol stream for DATA (seed 12345, as
// screamcheck wav) to STREAMOUT as little-endian uint32s.
func TestDumpStream(t *testing.T) {
	out := os.Getenv("STREAMOUT")
	if out == "" {
		t.Skip("set STREAMOUT and DATA")
	}
	payload, err := os.ReadFile(os.Getenv("DATA"))
	if err != nil {
		t.Fatal(err)
	}
	s := symbolStream(payload, 12345, envInt("SYMBOLS", 3600))
	b := make([]byte, 4*len(s))
	for i, v := range s {
		binary.LittleEndian.PutUint32(b[4*i:], v)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
