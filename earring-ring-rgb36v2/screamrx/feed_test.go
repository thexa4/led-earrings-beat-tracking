package screamrx

// On-target check of the decoder (firmware built with -tags screamfeed, see
// screamfeed.go): the simulator's windows go over SWD to the earring's
// decoder and to the same decoder here, and every result must agree bit for
// bit. This catches what the host tests cannot: TinyGo's code generation, the
// Goertzel assembly (the host links its C fallback) and the cycle budget.
//
//	FEED=localhost:4445 FEEDELF=../../captures/scream/screamfeed.elf \
//	    SCEN=headphone go test ./screamrx -run TestFeedTarget -v

import (
	"bufio"
	"debug/elf"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// ocd is a minimal OpenOCD telnet client, synchronised like tools/telem.py:
// every command is followed by a `puts` marker and the reply is what comes
// before it.
type ocd struct {
	conn net.Conn
	r    *bufio.Reader
	seq  int
}

func dialOCD(t *testing.T, addr string) *ocd {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	o := &ocd{conn: c, r: bufio.NewReaderSize(c, 1<<16)}
	o.until("> ")
	return o
}

func (o *ocd) until(marker string) string {
	o.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var b strings.Builder
	for !strings.HasSuffix(b.String(), marker) {
		c, err := o.r.ReadByte()
		if err != nil {
			panic(err)
		}
		b.WriteByte(c)
	}
	return strings.TrimSuffix(b.String(), marker)
}

func (o *ocd) cmd(line string) string {
	o.seq++
	m := fmt.Sprintf("###DONE%d", o.seq)
	fmt.Fprintf(o.conn, "%s\nputs %s\n", line, m)
	// The marker shows up in the echo of the puts command, right after the
	// first command's output (the puts output itself goes to the server log).
	return o.until(m)
}

var (
	hexWord  = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	wordLine = regexp.MustCompile(`^0x[0-9a-fA-F]+( 0x[0-9a-fA-F]+)*$`)
)

func (o *ocd) words(addr uint64, n int) []uint32 {
	reply := o.cmd(fmt.Sprintf("read_memory 0x%08x 32 %d", addr, n))
	var out []uint32
	for _, l := range strings.Split(reply, "\n") {
		// Only lines of nothing but words: the echo, and asynchronous
		// messages broadcast to every session (a halt from another client
		// prints the pc), are skipped.
		if !wordLine.MatchString(strings.Trim(l, "\x00\r ")) {
			continue
		}
		for _, h := range hexWord.FindAllString(l, -1) {
			v, _ := strconv.ParseUint(h[2:], 16, 32)
			out = append(out, uint32(v))
		}
	}
	if len(out) != n {
		panic(fmt.Sprintf("read_memory: %d/%d words: %q", len(out), n, reply))
	}
	return out
}

func elfSymbol(t *testing.T, path, name string) uint64 {
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	syms, err := f.Symbols()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range syms {
		if s.Name == name {
			return s.Value
		}
	}
	t.Fatalf("%s: no symbol %s", path, name)
	return 0
}

// feedCtl mirrors main.screamFeedCtl.
type feedCtl struct {
	Seq, Ack                        uint32
	Status, Adjust, Trim, Gain      int32
	Cycles, MaxCycles, Length, Hash uint32
	Stats                           Stats
}

const feedCtlWords = int(unsafe.Sizeof(feedCtl{}) / 4)

// feed drives the target decoder and a local one with the same windows.
type feed struct {
	t        *testing.T
	o        *ocd
	win, ctl uint64
	local    Decoder
	seq      uint32
	n        int
	cycles   []uint32
	mismatch int
	adj, rt  int
	last     feedCtl
}

func (f *feed) process(w []int32) int {
	if g := f.local.Gain(); g > 0 {
		for i := range w[:symLen] {
			w[i] >>= g
		}
	}
	var b strings.Builder
	for i := range symLen {
		// The window goes over as int16, so the local decoder sees the same.
		w[i] = max(-32768, min(32767, w[i]))
		fmt.Fprintf(&b, "%d ", uint16(int16(w[i])))
	}
	f.seq++
	f.o.cmd(fmt.Sprintf("write_memory 0x%08x 16 {%s}; mww 0x%08x %d", f.win, b.String(), f.ctl, f.seq))
	status := f.local.Process(w)
	f.adj, f.rt = f.local.Adjust(), f.local.RateTrim()
	var c feedCtl
	for range 200 {
		ws := f.o.words(f.ctl, feedCtlWords)
		c = *(*feedCtl)(unsafe.Pointer(&ws[0]))
		if c.Ack == f.seq {
			break
		}
	}
	if c.Ack != f.seq {
		f.t.Fatalf("window %d: target did not answer (ack %d)", f.n, c.Ack)
	}
	f.cycles = append(f.cycles, c.Cycles)
	f.last = c
	want := feedCtl{Seq: c.Seq, Ack: c.Ack, Status: int32(status), Adjust: int32(f.adj), Trim: int32(f.rt),
		Gain: int32(f.local.Gain()), Cycles: c.Cycles, MaxCycles: c.MaxCycles, Length: c.Length, Hash: c.Hash, Stats: f.local.Stats}
	if c != want && f.mismatch < 5 {
		f.mismatch++
		f.t.Errorf("window %d: target %+v\n  local %+v", f.n, c, want)
	}
	f.n++
	if status < 0 {
		f.local.Reset() // as screamfeed.go does
	}
	return status
}

func (f *feed) trim(cur int) int { return max(-20, min(20, cur+f.rt)) }
func (f *feed) bytes() []byte    { return f.local.Bytes() }
func (f *feed) reset()           {}
func (f *feed) adjust() int      { return f.adj }

func TestFeedTarget(t *testing.T) {
	addr := os.Getenv("FEED")
	if addr == "" {
		t.Skip("set FEED")
	}
	path := os.Getenv("FEEDELF")
	f := &feed{t: t, o: dialOCD(t, addr),
		win: elfSymbol(t, path, "main.adcData"), ctl: elfSymbol(t, path, "main.screamFeedCtl")}
	// Restart the target so both decoders start from Initialize.
	f.o.cmd("reset run")
	time.Sleep(500 * time.Millisecond)
	if acr := os.Getenv("FEEDACR"); acr != "" {
		// Override FLASH_ACR (e.g. 0x3: one wait state plus prefetch) to
		// measure its effect on the cycle count.
		f.o.cmd("mww 0x40022000 " + acr)
		t.Logf("FLASH_ACR %#x", f.o.words(0x40022000, 1)[0])
	}
	f.local.Initialize(make([]uint32, fountainWords), 1532, fs, 200*256/800)

	name := os.Getenv("SCEN")
	var sc scenario
	for _, s := range scenarios() {
		if s.name == name {
			sc = s
		}
	}
	if sc.build == nil {
		t.Fatalf("no scenario %q", name)
	}
	r := rand.New(rand.NewPCG(7, 7))
	data := make([]byte, envInt("SIZE", 698))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	stream := cachedStream(data, 0x1234, 90*50)
	tr := rand.New(rand.NewPCG(uint64(envInt("TRIAL", 0)), 99))
	ch := sc.build(tr)
	start := tr.Float64() * 2.5
	t0 := time.Now()
	// The target's fountain holds 436 words, as the firmware's does.
	res := run(f, ch, stream, data, start, float64(envInt("LIMIT", 30)), 799, tr)
	maxC := uint32(0)
	sum := 0.0
	for _, c := range f.cycles {
		maxC = max(maxC, c)
		sum += float64(c)
	}
	t.Logf("%s: received=%v after %.1fs; %d windows in %v; target cycles per window mean %.0f max %d (%.1f%% of a 20 ms symbol at 16 MHz)",
		name, res.ok, res.seconds, f.n, time.Since(t0).Round(time.Second), sum/float64(len(f.cycles)), maxC, float64(maxC)/320000*100)
	t.Logf("target stats %+v, received length %d hash %08x", f.last.Stats, f.last.Length, f.last.Hash)
	if f.mismatch > 0 {
		t.Errorf("%d windows disagreed", f.mismatch)
	}
}
