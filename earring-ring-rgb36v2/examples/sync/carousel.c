// Carousel: the music's palette turning round the ring.
//
// All 16 colors of the palette, spread once round the ring and blended into
// each other, turning all the time: faster the louder the mids are. Each
// time the music moves to a new section the ring turns the other way, a
// visible cue that the song moved on.
//
// The palette and the moments it turns back are the same on every earring;
// how far it has turned is each earring's own. Outside sync mode it turns
// slowly through a rainbow.
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

static uint32_t turn;          // how far it has turned, in 1/256 colors
static int direction;          // 1 or -1
static uint32_t last_sections; // SYNC->sections at the previous frame

// The palette at position pos (1/256 colors, wrapping after 16 colors),
// blended between its two nearest colors.
static uint32_t palette_at(const uint8_t *palette, uint32_t pos) {
  const uint8_t *a = palette + ((pos >> 8) & 15) * 3;
  const uint8_t *b = palette + (((pos >> 8) + 1) & 15) * 3;
  uint32_t w = pos & 0xff;
  uint32_t r = (a[0] * (256 - w) + b[0] * w) >> 8;
  uint32_t g = (a[1] * (256 - w) + b[1] * w) >> 8;
  uint32_t bl = (a[2] * (256 - w) + b[2] * w) >> 8;
  return r << 16 | g << 8 | bl;
}

void setup(int num) {
  direction = 1;
  if (sync_running()) {
    last_sections = SYNC->sections;
  }
}

void nextFrame(int millis) {
  uint32_t speed = 8; // in 1/256 colors per frame: 1 color a second
  if (sync_running()) {
    if (SYNC->sections != last_sections) {
      last_sections = SYNC->sections;
      direction = -direction;
    }
    speed = 8 + (SYNC->mid >> 2); // up to 8 colors a second
  }
  turn += direction * speed;
}

uint32_t getPixel(int index, int millis) {
  // index * 1820 >> 4 is index * 16 * 256 / 36: once round the palette.
  uint32_t pos = (index * 1820 >> 4) + turn;
  return palette_at(sync_running() ? SYNC->palette : rainbow, pos);
}
