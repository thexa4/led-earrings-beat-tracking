// Heartbeat: the whole ring breathes with the bass.
//
// The brightness is the bass level plus a flash on every beat that fades
// out until the next one (255 - beat_phase), added up and capped at full.
// Music without a clear kick still moves it, because the bass alone carries
// it. The palette turns round the ring, faster the louder the mids are, and
// the highs add white sparkles. A good default that always looks alive.
//
// The brightness follows the bass and the beat, so two earrings breathe
// together; the turning and the sparkles are each earring's own. Outside
// sync mode it breathes slowly in rainbow colors.
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

static uint32_t turn; // how far the palette has turned, in 1/256 colors

// The palette at position pos (1/256 colors, wrapping after 16 colors),
// blended between its two nearest colors, at brightness 0..255.
static uint32_t palette_at(const uint8_t *palette, uint32_t pos, uint32_t brightness) {
  const uint8_t *a = palette + ((pos >> 8) & 15) * 3;
  const uint8_t *b = palette + (((pos >> 8) + 1) & 15) * 3;
  uint32_t w = pos & 0xff;
  uint32_t r = (a[0] * (256 - w) + b[0] * w) >> 8;
  uint32_t g = (a[1] * (256 - w) + b[1] * w) >> 8;
  uint32_t bl = (a[2] * (256 - w) + b[2] * w) >> 8;
  return (r * brightness >> 8) << 16 | (g * brightness >> 8) << 8 | bl * brightness >> 8;
}

void setup(int num) {
}

void nextFrame(int millis) {
  // Up to 8 colors a second with the mids loud, 1 with them silent.
  turn += sync_running() ? 8 + (SYNC->mid >> 2) : 8;
}

uint32_t getPixel(int index, int millis) {
  // index * 1820 >> 4 is index * 16 * 256 / 36: once round the palette.
  uint32_t pos = (index * 1820 >> 4) + turn;
  if (!sync_running()) {
    uint32_t t = ((uint32_t)millis >> 2) & 511; // a breath every 2 s
    uint32_t breath = t < 256 ? t : 511 - t;
    return palette_at(rainbow, pos, 32 + (breath * 223 >> 8));
  }

  // Sparkles: up to one LED in eight, more the louder the highs, each for
  // two ticks (one reads as flicker).
  uint32_t high = SYNC->high;
  uint32_t x = ((uint32_t)index * 0x9e3779b1u ^ SYNC->frame >> 1) * 0x85ebca6bu;
  if ((x >> 24) < (high >> 3)) {
    return high * 0x010101;
  }

  uint32_t brightness = SYNC->bass + (255 - SYNC->beat_phase);
  if (brightness > 255) {
    brightness = 255;
  }
  return palette_at(SYNC->palette, pos, brightness);
}
