#include <stdint.h>

// bitplane[0] lo bits: 2 cycle bitplane
// bitplane[0] hi bits: 1 cycle bitplane
// bitplane[1] lo bits: 8 cycle bitplane
// bitplane[1] hi bits: 4 cycle bitplane
// bitplane[2] lo bits: 16 cycle bitplane

// Bitplanes for one group of three LEDs, from four pin slots:
//
//	a: PA15, PA10, PA9   b: PA8, PA7, PA6   c: PA5, PA4, PA3   d: PA2, PA1, PA0
//
// Each group drives three of the slots; the caller passes 0 (off) for the slot
// whose pins are that group's common line. The inputs are inverted up front
// instead of XORing each bitplane, so the off slot comes out as all ones, which
// is what the per-group versions produced by shifting in a gap and XORing it.
#define BB_ADC "adcs %[tmp], %[tmp], %[tmp]\n\t"
#define BB_ROT(r) "rors %[" r "], %[N8]\n\t"
#define BB_FIRST(r) "lsrs %[" r "], #4\n\t"
#define BB_NEXT(r) BB_ROT(r) "lsrs %[" r "], #9\n\t"
#define BB_SLOT(start, r, gap) start(r) BB_ADC gap BB_ROT(r) BB_ADC BB_ROT(r) BB_ADC
#define BB_PLANE(start) \
    BB_SLOT(start, "a", "lsls %[tmp], #4\n\t") /* gap between PA15 and PA10 */ \
    BB_SLOT(start, "b", "") BB_SLOT(start, "c", "") BB_SLOT(start, "d", "")

#define BB_OPERANDS \
    : [tmp]"+l"(tmp), [a]"+l"(a), [b]"+l"(b), [c]"+l"(c), [d]"+l"(d) \
    : [N8]"l"(N8)

static void bitbang_update_bitplane(uint32_t a, uint32_t b, uint32_t c, uint32_t d, uint32_t *bitplane) {
    uint32_t tmp = 0; // need not be: the 1 and 2 cycle bitplanes shift all 32 bits out
    uint32_t N8 = 8;
    a = ~a;
    b = ~b;
    c = ~c;
    d = ~d;

    __asm__ __volatile__(BB_PLANE(BB_FIRST) BB_OPERANDS); // 1 cycle bitplane
    // The 2, 4, 8 and 16 cycle bitplanes, the same code four times: a loop,
    // not unrolled (four copies are ~170 B more flash). A word is stored
    // after the 2, 8 and 16 cycle ones (the 16 cycle word's upper bits are
    // the 8 cycle bitplane).
#pragma clang loop unroll(disable)
    for (int i = 0; i < 4; i++) {
        __asm__ __volatile__(BB_PLANE(BB_NEXT) BB_OPERANDS);
        if (i != 1) {
            *bitplane++ = tmp;
        }
    }
}

// Updates all LEDs: one pass over the twelve groups of three, each showing its
// five bitplanes while its common line (the one pin of the mask that is
// push-pull, via OTYPER) is driven. The groups go in the order of their pins'
// bits, lowest first (PA0 .. PA10, PA15): the loop takes the lowest bit left
// in rem every pass, so bitplanes holds the groups in that order (see
// setLEDs). The bracketed numbers are cycles from the store before (stores
// to the GPIO, on the single-cycle I/O port, take one): every bitplane's on
// time is what it was when this was twelve copies of the group, and the loop
// bookkeeping sits in the 16 cycle bitplane's idle time. The back branch [2]
// is all-off time between groups.
static void bitbang_show_leds(volatile uint32_t *bitplanes, volatile void *gpio) {
    uint32_t v1, v2, rem;
    uint32_t mask = 0b1000011111111111;

    // Using the GPIO registers directly with memory offsets to avoid using one
    // more register:
    // OTYPER: [%[gpio], #0x4]
    // ODR:    [%[gpio], #0x14]

    __asm__ __volatile__(
        // prepare: OTYPER for the first group (PA0), its 8 and 4 cycle bitplanes
        "movs %[rem], %[mask]\n\t"
        "negs %[v1], %[rem]\n\t"
        "ands %[v1], %[rem]\n\t"             // lowest pin left
        "eors %[rem], %[v1]\n\t"
        "eors %[v1], %[mask]\n\t"            // OTYPER: clear only that pin
        "ldr  %[v2], [%[bitplanes], #4]\n\t"

        "1:\n\t"
        "strh %[v1], [%[gpio], #0x4]\n\t"    // set OTYPER for this group
        "strh %[v2], [%[gpio], #0x14]\n\t"   // [1] ** start 8 cycle bitplane
        "lsrs %[v2], #16\n\t"                // [1] move 4 cycle bitplane into lower bits
        "ldr  %[v1], [%[bitplanes], #0]\n\t" // [2] load 2 and 1 cycle bitplanes
        "nop\n\t"                            // [4] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[v1], [%[gpio], #0x14]\n\t"   // [1] ** start 2 cycle bitplane
        "lsrs %[v1], #16\n\t"                // [1] move 1 cycle bitplane into lower bits
        "strh %[v1], [%[gpio], #0x14]\n\t"   // [1] ** start 1 cycle bitplane
        "strh %[v2], [%[gpio], #0x14]\n\t"   // [1] ** start 4 cycle bitplane
        "ldr  %[v1], [%[bitplanes], #8]\n\t" // [2] load 16 cycle bitplane
        "nop\n\t"                            // [1] nop
        "strh %[v1], [%[gpio], #0x14]\n\t"   // [1] ** start 16 cycle bitplane
        "negs %[v1], %[rem]\n\t"             // [4] OTYPER for the next group
        "ands %[v1], %[rem]\n\t"
        "eors %[rem], %[v1]\n\t"
        "eors %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "cmp  %[v1], %[mask]\n\t"            // [1] no pin left: this was the last group
        "nop\n\t"                            // [7] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane
        "bne  1b\n\t"

        // restore: set OTYPER back to the mask
        "strh %[mask], [%[gpio], #0x4]\n\t"
        : [v1]"=&l"(v1),
          [v2]"=&l"(v2),
          [rem]"=&l"(rem),
          [bitplanes]"+&l"(bitplanes)
        : [gpio]"l"(gpio),
          [mask]"l"(mask)
        : "cc", "memory"
    );
}
