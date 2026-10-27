// Pulse: the colors the music picked, flashing on every beat.
//
// The simplest use of the sync data. Outside sync mode it shows a slow red
// chase instead, so it still does something as an ordinary custom pattern.
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
