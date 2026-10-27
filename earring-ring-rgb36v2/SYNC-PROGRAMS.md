# Using the music in your own patterns (party sync)

In party sync mode the earrings listen to the music and work out the beat,
how loud each part of the spectrum is, and which color scheme fits the
current section of the track. Several earrings hearing the same music come
to the same conclusions without talking to each other.

Your own [custom patterns](https://aykevl.nl/projects/earrings-rgb36v2#custom-patterns)
can use all of that too: run them from sync mode and read the values from a
small block of memory the earring keeps up to date. You can write the
pattern in the normal web editor; nothing about uploading changes.

## Running your pattern in sync mode

1. Upload your pattern to one of the two custom pattern modes as usual. If it
   has variants, pick the one you want there with the variant button.
2. Press the mode button until you reach sync mode. It comes right after the
   two custom pattern modes and starts with a colorful animation that pulses
   with the music.
3. Press the variant button to step through the sync mode variants:

   | variant | what it shows |
   |---|---|
   | 1 | **pulse** (the default): the music's color scheme, flashing on the beat, brighter with the bass, flowing faster with the mids, with glitter on the highs |
   | 2 | **classic**: the music's color scheme with a short flash on every beat |
   | 3 | **your pattern** from the first custom mode, with the music data |
   | 4 | **your pattern** from the second custom mode, with the music data |
| 5 | **interference pulse**: two rainbow waves running around the ring at slightly different speeds, with a white flash on the beat that fades out until the next one |
| 6 | **synthwave**: the ring strobes red and cyan, white for the first quarter of every beat |

When you switch to variant 3 or 4 the earring waits about a second before
it starts your pattern, just like the custom pattern modes do. If a slot is
empty you see a single blue LED.

Patterns that don't know about any of this run fine in sync mode too: they
just ignore the music. They also run exactly as before in the custom
pattern modes.

## Reading the music data

The earring keeps a block of values at the fixed memory address
`0x20001fc0`. In C (or C++, with FastLED), paste this block at the top of
your code, below `#include <stdint.h>`:

```c
// ---- Earring party sync data: paste this block as-is ----------------------
// The earring fills this in 64 times a second while sync mode runs this
// program. See SYNC-PROGRAMS.md for what every field means.
struct sync_data {
  uint32_t magic;         // SYNC_MAGIC when this firmware has sync data
  uint8_t version;        // 1 (or higher: later versions only add fields)
  uint8_t flags;          // SYNC_ACTIVE, SYNC_LOCKED, SYNC_SILENT
  uint8_t pattern;        // color scheme the music picked, 0..3
  uint8_t beat_phase;     // 0 on the beat, rising to 255 just before the next
  uint32_t frame;         // ticks (1/64 s) since sync mode started
  uint32_t beats;         // beats since sync mode started
  uint32_t sections;      // section changes since sync mode started
  uint16_t beat_ms;       // time between beats in ms, 0 while finding the tempo
  uint8_t level;          // overall loudness, 0..255
  uint8_t bass;           // below 250 Hz, 0..255
  uint8_t mid;            // 250 Hz to 1 kHz, 0..255
  uint8_t high;           // above 1.6 kHz, 0..255
  uint8_t section_age;    // ticks since the last section change (stops at 255)
  uint8_t beat_age;       // ticks since the last beat (stops at 255)
  uint8_t bands[8];       // loudness per band, 0..255: [0] lowest, [7] highest
  uint32_t hash;          // fingerprint of the current section of the music
  const uint8_t *palette; // the color scheme: 16 colors of 3 bytes (R, G, B)
};

#define SYNC_MAGIC  0x434e5953
#define SYNC_ACTIVE 1 // sync mode is running: everything above is live
#define SYNC_LOCKED 2 // the earring has found the tempo
#define SYNC_SILENT 4 // too quiet to hear any music

#define SYNC ((const volatile struct sync_data *)0x20001fc0)

// True when the values are live: firmware with sync data, running in sync
// mode. Otherwise (a custom pattern mode, or older firmware) don't use them.
static inline int sync_running(void) {
  return SYNC->magic == SYNC_MAGIC && (SYNC->flags & SYNC_ACTIVE);
}
// ---------------------------------------------------------------------------
```

Then read the values anywhere in your code, for example `SYNC->bass` or
`SYNC->bands[0]`. Check `sync_running()` first. It is false in the custom
pattern modes and on earrings with older firmware, and then the other
values mean nothing. A good pattern does something sensible in that case
too, so it still works as an ordinary custom pattern.

Here is a whole pattern
([`examples/sync/pulse.c`](examples/sync/pulse.c)): the music's colors
around the ring, flashing on every beat.

```c
#include <stdint.h>

// ... the sync data block from above ...

// Color number i (0..15) of the music's palette, at brightness 0..255.
static uint32_t palette_color(int i, int brightness) {
  const uint8_t *p = SYNC->palette + i * 3;
  uint32_t r = p[0] * brightness >> 8;
  uint32_t g = p[1] * brightness >> 8;
  uint32_t b = p[2] * brightness >> 8;
  return (r << 16) | (g << 8) | b;
}

void setup(int num) {
}

void nextFrame(int millis) {
}

uint32_t getPixel(int index, int millis) {
  if (!sync_running()) {
    // Not in sync mode: a slow red chase.
    return ((index * 7 + (millis >> 4)) & 0xff) << 16;
  }

  // Full brightness on the beat, fading out until the next one. Between
  // beats the bass keeps it up, so music without a steady kick still moves.
  int brightness = 255 - SYNC->beat_phase;
  if (SYNC->bass > brightness) {
    brightness = SYNC->bass;
  }

  // Spread the palette around the ring, and turn it a quarter every beat.
  // (index * 455 >> 10 is index * 16 / 36 without a division, which this
  // processor has no instruction for.)
  int color = (index * 455 >> 10) + (SYNC->beats & 3) * 4;
  color &= 15;
  return palette_color(color, brightness);
}
```

### What the values mean

| value | range | meaning |
|---|---|---|
| `flags` | bits | `SYNC_ACTIVE`: sync mode is running and the values are live. `SYNC_LOCKED`: the earring has found the tempo. `SYNC_SILENT`: too quiet to hear any music |
| `beat_phase` | 0..255 | where we are in the current beat: 0 on the beat, rising to 255 just before the next one. `255 - beat_phase` is a ready-made flash that fades out until the next beat |
| `beat_age` | 0..255 | ticks (1/64 s) since the last beat |
| `beat_ms` | ms | time between beats (60000 / `beat_ms` is the BPM), 0 while the earring is still finding the tempo |
| `beats` | count | beats since sync mode started |
| `pattern` | 0..3 | the color scheme the music picked for this section |
| `palette` | pointer | that color scheme: 16 colors of three bytes (red, green, blue). Color `i` is `palette[i*3]`, `palette[i*3+1]`, `palette[i*3+2]` |
| `section_age` | 0..255 | ticks since the music moved to a new section (a drop, a breakdown). `section_age < 2` is the moment itself, a good time for a big flash |
| `sections` | count | section changes since sync mode started |
| `hash` | bits | a fingerprint of the current section. It only changes when the section does, so it is good for picking colors that stay the same for a whole section |
| `level` | 0..255 | overall loudness |
| `bass`, `mid`, `high` | 0..255 | loudness below 250 Hz (kick drum, bass), 250 Hz to 1 kHz, and above 1.6 kHz (hi-hats, cymbals) |
| `bands[0..7]` | 0..255 | loudness of eight bands from low to high: below 250 Hz, 250-400 Hz, 400-630 Hz, 630 Hz-1 kHz, 1-1.6 kHz, 1.6-2.5 kHz, 2.5-4 kHz, above 4 kHz |
| `frame` | count | ticks since sync mode started, 64 per second |
| `magic`, `version` | | identify the block. `version` is 1. Later firmware may add values at the end and increase it, but never moves or removes these |

The loudness values follow the music within a few hundredths of a second.
Each band is measured against its own recent loudest moment, so a quiet
band still moves over the whole range. They drop to 0 when it is silent.

### What is the same on every earring

Earrings hearing the same music agree on `pattern`, `palette`, the timing
of the beats (`beat_phase`, `beat_age`), the moments the section changes
(`section_age`), and usually `hash`. Build your pattern from these and two
earrings will show the same thing at the same time.

The counters (`beats`, `sections`, `frame` and the `millis` your functions
get) start when each earring enters sync mode, so they differ between
earrings. Use them for things that may differ, like a slow rotation.
`beats & 3` is a fine way to do something every fourth beat on one earring,
but the earrings won't agree on which beat is the first. The loudness
values come from each earring's own microphone, so they are close but not
identical.

### Timing

In sync mode, `millis` counts real milliseconds since sync mode started (in
steps of 15.625 ms), and `nextFrame` is called 32 times a second.
`getPixel` is called more often than `nextFrame`, so it can be called
several times with the same `millis`. In the custom pattern modes nothing
changes: `millis` advances 37 per frame.

### Keep it fast

In sync mode, listening to the music takes about 60% of the earring's
processor (16 MHz). Your pattern shares what's left with the LEDs. If it
takes too much, the earring misses parts of the music and gets worse at
following the beat and the other earrings (the LEDs keep working). Measured
on the earrings:

| | measured | to be safe, stay under |
|---|---|---|
| one `getPixel` call | fine up to ~2900 cycles (180 µs) | 2000 cycles |
| one `nextFrame` call | fine at 60 000 cycles (4 ms); 185 000 lost 8% of the music | 60 000 cycles |
| one division or `%` | a few hundred cycles each | |

That is a lot of room for normal code: the three examples in
`examples/sync/` take 60-100 cycles per `getPixel` and 5-8% of the
processor. A loop that runs a few hundred times per pixel, or more than about
five divisions per pixel, is where it starts to go wrong. A program that
went over the limit with 16 divisions per pixel made the earring lose a third
of the music.

- Do slow work once per frame in `nextFrame`, store the result, and keep
  `getPixel` short. `getPixel` runs about 13 000 times a second, `nextFrame`
  32 times.
- Avoid division and `%` by anything other than a power of two. The
  processor has no divide instruction, so each one is a slow library call.
  Write `x * 455 >> 10` instead of `x * 16 / 36`, and `x & 3` instead of
  `x % 4`.
- There is no floating point hardware either: use integers.

## More examples

All of these are in [`examples/sync/`](examples/sync/). Copy the whole file
into the editor.

- [`pulse.c`](examples/sync/pulse.c): the one above.
- [`spectrum.c`](examples/sync/spectrum.c): an 8-band level meter mirrored
  on both halves of the ring, in the music's colors, with a white flash on
  every section change.
- [`hashview.c`](examples/sync/hashview.c): for checking several earrings
  against each other. The whole ring shows the current section's
  fingerprint as a color, and a blinking dot goes round once every four
  beats. Earrings that agree change color at the same moment and have dots
  moving at the same speed. This used to be built into sync mode.

### FastLED (C++)

The same block works in FastLED code. Paste it below the imports:

```cpp
import FastLED;
#include <stdint.h>

// ... the sync data block from above ...

void setup(int num) {
}

void nextFrame(int millis) {
}

CRGB getPixel(int index, int millis) {
  if (!sync_running()) {
    // Not in sync mode: the editor's default noise pattern.
    return ColorFromPalette(PartyColors_p, inoise16(index * 4096, millis * 20) >> 7);
  }
  // The music's colors around the ring, flashing on the beat.
  const uint8_t *p = SYNC->palette + (index * 455 >> 10) * 3;
  CRGB c(p[0], p[1], p[2]);
  c.nscale8(255 - SYNC->beat_phase);
  return c;
}
```

### Rust

```rust
#![no_std]

#[repr(C)]
#[derive(Clone, Copy)]
pub struct SyncData {
    pub magic: u32,
    pub version: u8,
    pub flags: u8,
    pub pattern: u8,
    pub beat_phase: u8,
    pub frame: u32,
    pub beats: u32,
    pub sections: u32,
    pub beat_ms: u16,
    pub level: u8,
    pub bass: u8,
    pub mid: u8,
    pub high: u8,
    pub section_age: u8,
    pub beat_age: u8,
    pub bands: [u8; 8],
    pub hash: u32,
    pub palette: *const [u8; 48],
}

pub const SYNC_ACTIVE: u8 = 1;
pub const SYNC_LOCKED: u8 = 2;
pub const SYNC_SILENT: u8 = 4;

/// The music data, when sync mode is running this pattern.
pub fn sync() -> Option<SyncData> {
    let s = unsafe { core::ptr::read_volatile(0x2000_1fc0 as *const SyncData) };
    if s.magic == 0x434e_5953 && s.flags & SYNC_ACTIVE != 0 {
        Some(s)
    } else {
        None
    }
}

pub fn setup(_num: i32) {}

pub fn next_frame(_millis: i32) {}

pub fn get_pixel(index: i32, millis: i32) -> u32 {
    let Some(s) = sync() else {
        // Not in sync mode: a slow red chase.
        return ((index as u32 * 7 + (millis as u32 >> 4)) & 0xff) << 16;
    };
    let brightness = (255 - s.beat_phase as u32).max(s.bass as u32);
    let i = (((index as u32 * 455) >> 10) + (s.beats & 3) * 4) as usize & 15;
    let p = unsafe { &*s.palette };
    let r = p[i * 3] as u32 * brightness >> 8;
    let g = p[i * 3 + 1] as u32 * brightness >> 8;
    let b = p[i * 3 + 2] as u32 * brightness >> 8;
    r << 16 | g << 8 | b
}
```

The FastLED and Rust versions were written to the same layout but could not
be compiled here, since the web editor's C++ and Rust toolchains aren't
available offline; the C examples were.

## For firmware developers

- The block is `syncShareT` in [`syncshare.go`](syncshare.go), filled in by
  `syncPoll` in [`sync.go`](sync.go) once per analysis frame. The address is
  the top 64 bytes of RAM, which [`stm32l031x6.ld`](stm32l031x6.ld) keeps
  out of the linker's RAM region. The address and the field offsets are an
  interface that uploaded programs depend on: never move or reorder
  anything, only append and increase `syncShareVersion`.
- The loudness values come from `Pipeline.Level` in
  [`syncdsp`](syncdsp/consts.go) (`LevelRangeQ8` and friends). Nothing the
  pipeline decides reads them, so they can be tuned for looks without
  affecting agreement between earrings.
- [`tools/patternc`](tools/patternc/main.go) builds a C pattern offline
  the way the web editor does, using the clang and lld inside TinyGo:

      go run ./tools/patternc -rt=false -o pulse.bin examples/sync/pulse.c

  `-rt=false` makes any division (any compiler-rt call) a link error, which
  is how the examples are checked to be division-free.
