// Ripples: every beat drops a stone onto the ring.
//
// On each beat a wavefront starts at one LED and runs both ways round the
// ring, getting dimmer as it goes, and meets itself on the far side just as
// the next beat comes. At fast tempos ripples overlap: up to three are in
// flight at once. Each one has the palette color of the LED it started on,
// and the starting point moves 13 LEDs every beat, so they come from all
// over the ring.
//
// The ripples start on the beats, which all earrings agree on; where they
// start is each earring's own. Outside sync mode a blue ripple starts every
// half second.
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

#define RIPPLES 3

// The ripples in flight: the tick (1/64 s) each one started at and its LED.
// LED 0xff marks a free slot.
static uint32_t ripple_start[RIPPLES];
static uint8_t ripple_led[RIPPLES];
static int next_slot;
static int origin;          // where the next ripple starts
static uint32_t last_beats; // SYNC->beats at the previous frame
static uint32_t last_start; // outside sync mode: when the last one started
static uint32_t life;       // how long a ripple lasts, in ticks: one beat

// The clock ripples run on: sync mode's ticks, or the same speed from millis.
static uint32_t now(int millis) {
  return sync_running() ? SYNC->frame : (uint32_t)millis >> 4;
}

// Palette color i (0..15) at brightness 0..255; blue outside sync mode.
static uint32_t color(int i, uint32_t brightness) {
  uint32_t r = 0, g = 64, b = 255;
  if (sync_running()) {
    const uint8_t *p = SYNC->palette + i * 3;
    r = p[0];
    g = p[1];
    b = p[2];
  }
  return (r * brightness >> 8) << 16 | (g * brightness >> 8) << 8 | b * brightness >> 8;
}

static void start_ripple(uint32_t tick) {
  ripple_start[next_slot] = tick;
  ripple_led[next_slot] = origin;
  next_slot = next_slot == RIPPLES - 1 ? 0 : next_slot + 1;
  origin += 13; // 13 and 36 have no common factor: every LED gets a turn
  if (origin >= 36) {
    origin -= 36;
  }
}

void setup(int num) {
  for (int i = 0; i < RIPPLES; i++) {
    ripple_led[i] = 0xff;
  }
  life = 32;
}

void nextFrame(int millis) {
  if (!sync_running()) {
    life = 32;
    if (now(millis) - last_start >= 32) {
      last_start = now(millis);
      start_ripple(last_start);
    }
    return;
  }

  // nextFrame runs 32 times a second and the sync data changes 64 times a
  // second, so it can miss the tick where beat_age is 0. Watch the beat
  // count instead, and date the ripple back to the beat itself.
  if (SYNC->beats != last_beats) {
    last_beats = SYNC->beats;
    start_ripple(SYNC->frame - SYNC->beat_age);
  }

  // One beat in ticks is beat_ms * 64 / 1000: beat_ms * 33 >> 9 without a
  // division. Half a second while the earring is still finding the tempo.
  life = SYNC->beat_ms ? SYNC->beat_ms * 33 >> 9 : 32;
}

uint32_t getPixel(int index, int millis) {
  uint32_t t = now(millis);
  uint32_t best = 0;
  int best_led = 0;
  for (int i = 0; i < RIPPLES; i++) {
    if (ripple_led[i] == 0xff) {
      continue;
    }
    uint32_t age = t - ripple_start[i];
    if (age >= life) {
      continue;
    }

    // Distance round the ring from where the ripple started: 0..18.
    int d = index - ripple_led[i];
    if (d < 0) {
      d = -d;
    }
    if (d > 18) {
      d = 36 - d;
    }

    // The front is age * 18 / life LEDs out. Compare without dividing: the
    // front is on this LED when d * life <= age * 18 < (d + 1) * life. The
    // LED just behind it glows a little too.
    uint32_t front = age * 18;
    uint32_t brightness;
    if (front >= d * life && front < (d + 1) * life) {
      brightness = 255 - d * 13;
    } else if (front >= (d + 1) * life && front < (d + 2) * life) {
      brightness = (255 - d * 13) >> 2;
    } else {
      continue;
    }
    if (brightness > best) {
      best = brightness;
      best_led = ripple_led[i];
    }
  }
  if (best == 0) {
    return 0;
  }
  // index * 455 >> 10 is index * 16 / 36: the palette spread round the ring.
  return color(best_led * 455 >> 10, best);
}
