// Mosaic: the section's fingerprint as six tiles of color.
//
// The ring is six tiles of six LEDs. Each tile takes four bits of the
// section's hash as a palette color: tile 0 bits 0-3, tile 1 bits 4-7, and
// so on. The hash only changes with the section, so the ring is a fixed
// mosaic for a whole section, and all six tiles change at once when the
// music moves on.
//
// The hash and the palette are the same on every earring, so a pair shows
// the same mosaic. Outside sync mode the tiles change every four seconds,
// in rainbow colors.
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

// The colors to use outside sync mode: FastLED's rainbow.
static const uint8_t rainbow[48] = {
  0xff, 0x00, 0x00, 0xd5, 0x2a, 0x00, 0xab, 0x55, 0x00, 0xab, 0x7f, 0x00,
  0xab, 0xab, 0x00, 0x56, 0xd5, 0x00, 0x00, 0xff, 0x00, 0x00, 0xd5, 0x2a,
  0x00, 0xab, 0x55, 0x00, 0x56, 0xaa, 0x00, 0x00, 0xff, 0x2a, 0x00, 0xd5,
  0x55, 0x00, 0xab, 0x7f, 0x00, 0x81, 0xab, 0x00, 0x55, 0xd5, 0x00, 0x2b,
};

void setup(int num) {
}

void nextFrame(int millis) {
}

uint32_t getPixel(int index, int millis) {
  uint32_t hash = ((uint32_t)millis >> 12) * 0x9e3779b1u;
  const uint8_t *palette = rainbow;
  if (sync_running()) {
    hash = SYNC->hash;
    palette = SYNC->palette;
  }
  int tile = index * 43 >> 8; // index / 6 without a division: 0..5
  const uint8_t *p = palette + ((hash >> (tile * 4)) & 15) * 3;
  return p[0] << 16 | p[1] << 8 | p[2];
}
