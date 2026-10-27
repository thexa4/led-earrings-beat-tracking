// History: a map of the set you have been listening to.
//
// Every time the music moves to a new section, that section's color (its
// hash picks one of the palette's 16) is pushed onto the ring at the top,
// LED 0, and everything else moves one LED along. After a few minutes the
// ring shows the last 36 sections, the newest at the top beating with the
// music. The map keeps palette entries, so it is drawn in the palette of
// the moment.
//
// Two earrings don't show the same map: each starts its own when it enters
// sync mode. The map takes 36 bytes, which count toward the program size
// (globals are part of the program). Outside sync mode it fills with
// rainbow colors, one every two seconds.
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

static uint8_t history[36];    // palette entries, 0xff for none yet
static int newest;             // where in history the newest entry is
static uint32_t last_sections; // SYNC->sections at the previous frame
static uint32_t last_push;     // outside sync mode: millis of the last push

// Push a palette entry: the newest moves to LED 0, the rest one LED along.
static void push(int entry) {
  newest = newest == 0 ? 35 : newest - 1;
  history[newest] = entry;
}

void setup(int num) {
  for (int i = 0; i < 36; i++) {
    history[i] = 0xff;
  }
  if (sync_running()) {
    last_sections = SYNC->sections;
    push(SYNC->hash & 15); // the section playing now
  }
}

void nextFrame(int millis) {
  if (sync_running()) {
    if (SYNC->sections != last_sections) {
      last_sections = SYNC->sections;
      push(SYNC->hash & 15);
    }
  } else if ((uint32_t)millis - last_push >= 2000) {
    last_push = millis;
    push((millis >> 11) & 15);
  }
}

uint32_t getPixel(int index, int millis) {
  int i = newest + index;
  if (i >= 36) {
    i -= 36;
  }
  if (history[i] == 0xff) {
    return 0;
  }
  const uint8_t *p = (sync_running() ? SYNC->palette : rainbow) + history[i] * 3;

  // The older sections at 5/8 brightness, the newest beating.
  uint32_t brightness = 160;
  if (index == 0 && sync_running()) {
    brightness = 255 - (SYNC->beat_phase * 159 >> 8);
  }
  return (p[0] * brightness >> 8) << 16 | (p[1] * brightness >> 8) << 8 | p[2] * brightness >> 8;
}
