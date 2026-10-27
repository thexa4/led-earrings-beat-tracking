// Spectrum: a mirrored 8-band level meter in the music's colors.
//
// Lowest band at LED 0, highest at LED 18, on both sides of the ring. A white
// flash marks every section change. Outside sync mode it glows dim blue.
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

void setup(int num) {
}

void nextFrame(int millis) {
}

uint32_t getPixel(int index, int millis) {
  if (!sync_running()) {
    return 0x000010;
  }
  if (SYNC->section_age < 2) {
    return 0xffffff; // a new section of the music just started
  }

  // Position from LED 0, the same on both halves: 0..18.
  int pos = index <= 18 ? index : 36 - index;
  int band = pos * 27 >> 6; // 0..7: pos * 8 / 19 without a division
  int level = SYNC->bands[band];

  // Band n takes color 2n of the palette.
  const uint8_t *p = SYNC->palette + band * 6;
  uint32_t r = p[0] * level >> 8;
  uint32_t g = p[1] * level >> 8;
  uint32_t b = p[2] * level >> 8;
  return (r << 16) | (g << 8) | b;
}
