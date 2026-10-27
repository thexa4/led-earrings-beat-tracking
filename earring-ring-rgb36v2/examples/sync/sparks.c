// Sparks: the hi-hats throw white sparks onto the ring.
//
// Only the top of the spectrum counts. Whenever the highs (high and the two
// top bands) jump up since the last frame, 3 to 5 LEDs light up white and
// fade out in a fifth of a second. All the work is in nextFrame; getPixel
// only compares against five bytes, so this costs next to nothing.
//
// The hits are the same on every earring, the LEDs they land on are not.
// Outside sync mode sparks come at random, about twice a second.
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

#define SPARKS 5
#define JUMP 120 // how far the highs (0..510) must jump in one frame

static uint8_t spark[SPARKS]; // the LEDs sparking now, 0xff for none
static uint8_t spark_level;   // their brightness, fading out
static uint32_t seed;
static uint32_t last_highs;

static uint32_t next_random(void) {
  seed = seed * 1103515245u + 12345u;
  return seed;
}

static void burst(void) {
  int count = 3 + ((next_random() >> 24) * 3 >> 8); // 3..5
  for (int i = 0; i < SPARKS; i++) {
    spark[i] = i < count ? (next_random() >> 24) * 36 >> 8 : 0xff;
  }
  spark_level = 255;
}

void setup(int num) {
  for (int i = 0; i < SPARKS; i++) {
    spark[i] = 0xff;
  }
}

void nextFrame(int millis) {
  spark_level = spark_level > 40 ? spark_level - 40 : 0; // 6 frames

  if (sync_running()) {
    uint32_t highs = SYNC->high + ((SYNC->bands[6] + SYNC->bands[7]) >> 1);
    if (highs > last_highs + JUMP) {
      burst();
    }
    last_highs = highs;
  } else if (next_random() >> 24 < 20) {
    burst();
  }
}

uint32_t getPixel(int index, int millis) {
  for (int i = 0; i < SPARKS; i++) {
    if (spark[i] == index) {
      return spark_level * 0x010101;
    }
  }
  return 0;
}
