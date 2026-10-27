// Hash view: check several earrings against each other at a glance.
//
// This was a built-in sync mode variant; it is a program now.
//
//  - The whole ring shows one color, the current section's fingerprint. It
//    changes when an earring decides the music moved to a new section, with
//    one white flash. Earrings that agree change to the same color at the
//    same moment.
//  - A dot goes a quarter of the way round per beat (a lap per bar),
//    blinking black and white. Earrings with dots chasing at the same speed
//    have found the same tempo.
//  - Dim grey: still finding the tempo. Dark: silence, or not in sync mode.
#include <stdint.h>

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

// Hue 0..255 (0 red, 85 green, 170 blue) at full saturation and brightness.
static uint32_t hue(int h) {
  int region = h * 6 >> 8;
  int f = h * 6 & 0xff;
  int q = 255 - f;
  int t = f;
  switch (region) {
  case 0:
    return 0xff0000 | t << 8;
  case 1:
    return q << 16 | 0x00ff00;
  case 2:
    return 0x00ff00 | t;
  case 3:
    return q << 8 | 0x0000ff;
  case 4:
    return t << 16 | 0x0000ff;
  default:
    return 0xff0000 | q;
  }
}

static uint32_t mix(uint32_t a, uint32_t b, int w) { // w = 0..256 of b
  uint32_t out = 0;
  for (int shift = 0; shift < 24; shift += 8) {
    uint32_t ca = (a >> shift) & 0xff;
    uint32_t cb = (b >> shift) & 0xff;
    out |= ((ca * (256 - w) + cb * w) >> 8) << shift;
  }
  return out;
}

void setup(int num) {
}

void nextFrame(int millis) {
}

uint32_t getPixel(int index, int millis) {
  if (!sync_running() || (SYNC->flags & SYNC_SILENT)) {
    return 0;
  }
  if (!(SYNC->flags & SYNC_LOCKED)) {
    return 0x181818;
  }
  if (SYNC->section_age < 2) {
    return 0xffffff;
  }
  uint32_t h = SYNC->hash;
  uint32_t color = hue((h ^ (h >> 9) ^ (h >> 18)) & 0xff);

  // Dot position in 1/256 LEDs: 9 LEDs per beat, sliding between beats, a
  // lap every 4 beats (at most 3 * 9 * 256 + 9 * 255, still inside the ring).
  uint32_t dot = (SYNC->beats & 3) * 9 * 256 + 9 * SYNC->beat_phase;
  int first = dot >> 8;
  int frac = dot & 0xff;
  int w = 0;
  if (index == first) {
    w = 256 - frac;
  } else if (index == first + 1 || (first == 35 && index == 0)) {
    w = frac;
  }
  if (w == 0) {
    return color;
  }
  uint32_t blink = (SYNC->frame >> 2) & 1 ? 0 : 0xffffff; // 8 times a second
  return mix(color, blink, w);
}
