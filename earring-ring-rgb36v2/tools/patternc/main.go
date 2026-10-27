// Command patternc builds a C custom pattern offline, the way the web editor
// (https://aykevl.nl/apps/led-editor/) does, so that example programs can be
// compiled and checked without the editor's server:
//
//	go run ./tools/patternc -o pulse.bin examples/sync/pulse.c
//	go run ./tools/patternc -variants 3 -o x.bin x.c
//	go run ./tools/patternc -rt=false -o x.bin x.c   # no divisions etc.
//
// It uses the clang and lld that ship inside TinyGo (`tinygo clang`,
// `tinygo ld.lld`) with the editor's flags (-O2 -Wall -Werror), its linker
// script (language/link.ld in led-editor, copied below) and a port of its
// elfToBinary (server.go). The editor's toolchain configuration file is not
// public, so code generation may differ in detail; what matters here - the
// header, the relocation rules (only R_ARM_ABS32 at word offsets) and that
// the source compiles warning-free - is the same.
//
// The output is the pattern binary the earring receives (scream/ENCODING.md
// §6). patternc prints its size and relocation count; it fails on anything
// the editor would refuse.
package main

import (
	"debug/elf"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// linkLD is led-editor's language/link.ld.
const linkLD = `MEMORY {
    DATA (rwx) : ORIGIN = 0, LENGTH = 128K
}

ENTRY(__getPixel)

SECTIONS
{
    .header :
    {
        LONG(0xf95ac4b4)
        BYTE(1)
        BYTE(__flags)
        BYTE(__shape)
        BYTE(1)
        SHORT(0)
        SHORT(0)
        SHORT(__setup|1)
        SHORT(__nextFrame|1)
        SHORT(__getPixel|1)
        SHORT(0)
    }

    .variants :
    {
      __variant = .+1;
      . += (__flags & 1) ? 2 : 0;
    } >DATA

    .code :
    {
        *(.text)
        *(.text.*)
        *(.rodata)
        *(.rodata.*)
        *(.data)
        *(.data.*)
        *(.bss)
        *(.bss.*)
    } > DATA

    /DISCARD/ :
    {
        *(.ARM.exidx)
        *(.ARM.exidx.*)
    }
}
`

func main() {
	out := flag.String("o", "", "output pattern binary")
	variants := flag.Int("variants", 1, "number of variants (the editor's variant setting)")
	useRT := flag.Bool("rt", true, "link compiler-rt (false: fail on any libcall, e.g. a division)")
	flag.Parse()
	if *out == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: patternc [-variants n] -o out.bin file.c...")
		os.Exit(2)
	}
	if err := build(*out, *variants, *useRT, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "patternc:", err)
		os.Exit(1)
	}
}

func build(out string, variants int, useRT bool, srcs []string) error {
	tmp, err := os.MkdirTemp("", "patternc-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	root, err := exec.Command("tinygo", "env", "TINYGOROOT").Output()
	if err != nil {
		return fmt.Errorf("tinygo env: %w", err)
	}
	include := filepath.Join(strings.TrimSpace(string(root)), "lib", "clang", "include")

	var objs []string
	for i, src := range srcs {
		obj := filepath.Join(tmp, fmt.Sprintf("%d.o", i))
		if err := run("tinygo", "clang", "--target=armv6m-unknown-none-eabi", "-mthumb",
			"-mcpu=cortex-m0plus", "-mfloat-abi=soft", "-ffreestanding", "-nostdlibinc",
			"-isystem", include, "-O2", "-Wall", "-Werror", "-c", "-o", obj, src); err != nil {
			return err
		}
		objs = append(objs, obj)
	}

	ld := filepath.Join(tmp, "link.ld")
	if err := os.WriteFile(ld, []byte(linkLD), 0o644); err != nil {
		return err
	}
	flags := 0
	args := []string{"ld.lld", "-o", filepath.Join(tmp, "binary.elf"), "-T", ld,
		"--gc-sections", "--emit-relocs", "--defsym=__shape=1",
		"--defsym=__setup=setup", "--defsym=__nextFrame=nextFrame", "--defsym=__getPixel=getPixel"}
	if variants > 1 {
		flags |= 1
		args = append(args, "--defsym=variant=__variant")
	}
	args = append(args, fmt.Sprintf("--defsym=__flags=%d", flags))
	// The editor links its sysroot's compiler-rt; TinyGo's build cache has
	// one for this CPU once any firmware was built.
	if home, err := os.UserCacheDir(); err == nil && useRT {
		rt := filepath.Join(home, "tinygo", "compiler-rt-thumbv6m-unknown-unknown-eabi-cortex-m0plus", "lib.a")
		if _, err := os.Stat(rt); err == nil {
			objs = append(objs, rt)
		}
	}
	if err := run("tinygo", append(args, objs...)...); err != nil {
		return err
	}

	bin, nrelocs, err := elfToBinary(filepath.Join(tmp, "binary.elf"), variants)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, bin, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %d bytes, %d relocations (max 1532 bytes)\n", out, len(bin), nrelocs)
	if len(bin) > 1532 {
		return fmt.Errorf("%s: too big for a pattern slot", out)
	}
	return nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, args[0], err)
	}
	return nil
}

// elfToBinary is led-editor's server.go elfToBinary.
func elfToBinary(elffile string, variants int) ([]byte, int, error) {
	file, err := elf.Open(elffile)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	start, end := ^uint64(0), uint64(0)
	var progs []*elf.Prog
	for _, prog := range file.Progs {
		if prog.Type != elf.PT_LOAD {
			continue
		}
		progs = append(progs, prog)
		start = min(start, prog.Vaddr)
		end = max(end, prog.Vaddr+prog.Filesz)
	}
	imageSize := (end - start + 1) &^ 1

	var relocs []uint16
	for _, section := range file.Sections {
		if section.Type != elf.SHT_REL || file.Sections[section.Info].Flags&elf.SHF_ALLOC == 0 {
			continue
		}
		data, err := section.Data()
		if err != nil {
			return nil, 0, err
		}
		for i := 0; i+8 <= len(data); i += 8 {
			off := binary.LittleEndian.Uint32(data[i:])
			typ := elf.R_ARM(elf.R_TYPE32(binary.LittleEndian.Uint32(data[i+4:])))
			switch typ {
			case elf.R_ARM_THM_PC22:
			case elf.R_ARM_ABS32:
				if off%4 != 0 || off/4 > 0xffff {
					return nil, 0, fmt.Errorf("could not relocate %s at %d: offset out of range", typ, off)
				}
				relocs = append(relocs, uint16(off/4))
			default:
				return nil, 0, fmt.Errorf("could not relocate %s at %d: unknown relocation (the editor refuses this too)", typ, off)
			}
		}
	}

	output := make([]byte, imageSize+uint64(len(relocs)*2))
	for _, prog := range progs {
		if prog.Filesz == 0 {
			continue
		}
		o := prog.Vaddr - start
		if _, err := prog.ReadAt(output[o:o+prog.Filesz], 0); err != nil {
			return nil, 0, err
		}
	}
	binary.LittleEndian.PutUint16(output[18:20], uint16(imageSize/2))
	for i, addr := range relocs {
		binary.LittleEndian.PutUint16(output[imageSize+uint64(i)*2:], addr)
	}
	if mv := min(max(variants-1, 0), 255); mv > 0 && len(output) >= 22 {
		output[20] = uint8(mv)
	}
	return output, len(relocs), nil
}
