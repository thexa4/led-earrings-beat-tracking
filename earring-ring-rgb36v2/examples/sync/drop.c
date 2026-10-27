// Drop: every new section goes off like a bomb, then settles.
//
// When the music moves to a new section (a drop, a breakdown) the whole
// ring flashes white. Then a wavefront runs from the top both ways round
// the ring and leaves the palette behind it at full color. After that the
// colors slowly drain towards grey until the next section, so the arc of
// the song - build, drop, settle - shows on the ring. The ring also pulses
// gently on the beat.
//
// It all follows the section changes, the beats and the palette, which
// every earring agrees on, so a pair goes off together. Outside sync mode
// it goes off every eight seconds, in rainbow colors.
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

static uint32_t last_sections;       // the section count at the previous frame
static uint32_t section_start;       // the tick (1/64 s) the section started
static const uint8_t *palette;       // the palette now
static uint32_t saturation;          // the saturation now, 0..255
static const uint8_t *old_palette;   // ahead of the wavefront: the old look
static uint32_t old_saturation;

// Outside sync mode a timer stands in for the music: a section every 8 s,
// ticks from millis.
static uint32_t sections(int millis) {
  return sync_running() ? SYNC->sections : (uint32_t)millis >> 13;
}

static uint32_t ticks(int millis) {
  return sync_running() ? SYNC->frame : (uint32_t)millis >> 4;
}

// Palette color i (0..15), mixed towards its own grey by saturation and
// scaled by brightness (both 0..255).
static uint32_t paint(const uint8_t *pal, int i, uint32_t sat, uint32_t brightness) {
  const uint8_t *p = pal + i * 3;
  uint32_t grey = (p[0] + p[1] + p[2]) * 85 >> 8;
  uint32_t r = (p[0] * sat + grey * (256 - sat)) >> 8;
  uint32_t g = (p[1] * sat + grey * (256 - sat)) >> 8;
  uint32_t b = (p[2] * sat + grey * (256 - sat)) >> 8;
  return (r * brightness >> 8) << 16 | (g * brightness >> 8) << 8 | b * brightness >> 8;
}

void setup(int num) {
  last_sections = sections(0);
  palette = sync_running() ? SYNC->palette : rainbow;
  old_palette = palette;
  saturation = 255;
  old_saturation = 255;
}

void nextFrame(int millis) {
  uint32_t now = ticks(millis);
  if (sections(millis) != last_sections) {
    // What was on the ring stays ahead of the wavefront.
    last_sections = sections(millis);
    old_palette = palette;
    old_saturation = saturation;
    // nextFrame runs every other tick: date the start back to the change.
    section_start = sync_running() ? now - SYNC->section_age : now;
  }
  palette = sync_running() ? SYNC->palette : rainbow;

  // Drain the color: from full at the drop to 1/8 after 14 s. (section_age
  // stops at 255 ticks, 4 s, so this counts its own ticks.)
  uint32_t drained = (now - section_start) >> 2;
  saturation = 255 - (drained < 224 ? drained : 224);
}

uint32_t getPixel(int index, int millis) {
  uint32_t age = ticks(millis) - section_start;
  if (sync_running() ? SYNC->section_age < 2 : age < 2) {
    return 0xffffff; // the flash, two ticks
  }

  // The wavefront runs from LED 0 both ways at half an LED per tick, so it
  // reaches the bottom 36 ticks (just over half a second) after the flash.
  uint32_t d = index <= 18 ? index : 36 - index;
  uint32_t front = (age - 2) >> 1;
  if (d == front) {
    return 0xc0c0c0;
  }

  uint32_t brightness = 255;
  if (sync_running()) {
    brightness = 200 + ((255 - SYNC->beat_phase) * 55 >> 8);
  }
  int color = index * 455 >> 10; // index * 16 / 36: the palette round the ring
  if (d < front) {
    return paint(palette, color, saturation, brightness);
  }
  return paint(old_palette, color, old_saturation, brightness);
}
