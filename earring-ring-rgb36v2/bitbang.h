#include <stdint.h>

// bitplane[0] lo bits: 2 cycle bitplane
// bitplane[0] hi bits: 1 cycle bitplane
// bitplane[1] lo bits: 8 cycle bitplane
// bitplane[1] hi bits: 4 cycle bitplane
// bitplane[2] lo bits: 16 cycle bitplane

static void bitbang_update_bitplane_1(uint32_t led0, uint32_t led1, uint32_t led2, uint32_t *bitplane) {
    uint32_t tmp;
    uint32_t N8 = 8;
    uint32_t xor = 0b1000011111111111;

    __asm__ __volatile__(
        // Not clearing %[tmp] here since we will be shifting all 32 bits out.

        // 1 cycle bitplane
        "lsrs %[led0], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "lsls %[tmp], #3\n\t"             // gap between PA9 and PA5
        "lsrs %[led2], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A7/PA5)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A8/PA4)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A9/PA3)
        "lsrs %[led1], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A10/PA2)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A11/PA1)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // 2 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "lsls %[tmp], #3\n\t"             // gap between PA9 and PA5
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A7/PA5)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A8/PA4)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A9/PA3)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A10/PA2)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A11/PA1)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // Store 1-cycle and 2-cycle bitplanes.
        "str %[tmp], [%[bitplane]]\n\t"

        // Not clearing %[tmp] here since we will be shifting all 32 bits out.

        // 4 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "lsls %[tmp], #3\n\t"             // gap between PA9 and PA5
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A7/PA5)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A8/PA4)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A9/PA3)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A10/PA2)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A11/PA1)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // 8 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "lsls %[tmp], #3\n\t"             // gap between PA9 and PA5
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A7/PA5)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A8/PA4)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A9/PA3)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A10/PA2)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A11/PA1)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // Store 4-cycle and 8-cycle bitplanes.
        "str %[tmp], [%[bitplane], #4]\n\t"

        // 16 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "lsls %[tmp], #3\n\t"             // gap between PA9 and PA5
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A7/PA5)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A8/PA4)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A9/PA3)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A10/PA2)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A11/PA1)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // Store 16-cycle bitplane (of which the upper bits weren't cleared).
        "str %[tmp], [%[bitplane], #8]\n\t"

        : [tmp]"=&r"(tmp),
          [led0]"+&r"(led0),
          [led1]"+&r"(led1),
          [led2]"+&r"(led2)
        : [bitplane]"r"(bitplane),
          [N8]"r"(N8),
          [xor]"r"(xor)
    );
}

static void bitbang_update_bitplane_2(uint32_t led0, uint32_t led1, uint32_t led2, uint32_t *bitplane) {
    uint32_t tmp;
    uint32_t N8 = 8;
    uint32_t xor = 0b1000011111111111;

    __asm__ __volatile__(
        // Not clearing %[tmp] here since we will be shifting all 32 bits out.

        // 1 cycle bitplane
        "lsrs %[led0], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "lsrs %[led2], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "lsls %[tmp], #3\n\t"             // gap between PA6 and PA2
        "lsrs %[led1], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A10/PA2)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A11/PA1)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // 2 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "lsls %[tmp], #3\n\t"             // gap between PA6 and PA2
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A10/PA2)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A11/PA1)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // Store 1-cycle and 2-cycle bitplanes.
        "str %[tmp], [%[bitplane]]\n\t"

        // Not clearing %[tmp] here since we will be shifting all 32 bits out.

        // 4 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "lsls %[tmp], #3\n\t"             // gap between PA6 and PA2
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A10/PA2)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A11/PA1)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // 8 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "lsls %[tmp], #3\n\t"             // gap between PA6 and PA2
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A10/PA2)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A11/PA1)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // Store 4-cycle and 8-cycle bitplanes.
        "str %[tmp], [%[bitplane], #4]\n\t"

        // 16 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "lsls %[tmp], #3\n\t"             // gap between PA6 and PA2
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A10/PA2)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A11/PA1)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // Store 16-cycle bitplane (of which the upper bits weren't cleared).
        "str %[tmp], [%[bitplane], #8]\n\t"

        : [tmp]"=&r"(tmp),
          [led0]"+&r"(led0),
          [led1]"+&r"(led1),
          [led2]"+&r"(led2)
        : [bitplane]"r"(bitplane),
          [N8]"r"(N8),
          [xor]"r"(xor)
    );
}

static void bitbang_update_bitplane_3(uint32_t led0, uint32_t led1, uint32_t led2, uint32_t *bitplane) {
    uint32_t tmp;
    uint32_t N8 = 8;
    uint32_t xor = 0b1000011111111111;

    __asm__ __volatile__(
        // Not clearing %[tmp] here since we will be shifting all 32 bits out.

        // 1 cycle bitplane
        "lsrs %[led0], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "lsrs %[led2], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "lsrs %[led1], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A7/PA5)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A8/PA4)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A9/PA3)
        "lsls %[tmp], #3\n\t"             // gap at the start (PA3-0)
        "eors %[tmp], %[xor]\n\t"

        // 2 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A7/PA5)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A8/PA4)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A9/PA3)
        "lsls %[tmp], #3\n\t"             // gap at the start (PA3-0)
        "eors %[tmp], %[xor]\n\t"

        // Store 1-cycle and 2-cycle bitplanes.
        "str %[tmp], [%[bitplane]]\n\t"

        // Not clearing %[tmp] here since we will be shifting all 32 bits out.

        // 4 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A7/PA5)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A8/PA4)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A9/PA3)
        "lsls %[tmp], #3\n\t"             // gap at the start (PA3-0)
        "eors %[tmp], %[xor]\n\t"

        // 8 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A7/PA5)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A8/PA4)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A9/PA3)
        "lsls %[tmp], #3\n\t"             // gap at the start (PA3-0)
        "eors %[tmp], %[xor]\n\t"

        // Store 4-cycle and 8-cycle bitplanes.
        "str %[tmp], [%[bitplane], #4]\n\t"

        // 16 cycle bitplane
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A1/PA15)
        "lsls %[tmp], #4\n\t"             // gap between PA15 and PA10
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A2/PA10)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A3/PA9)
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A7/PA5)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A8/PA4)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A9/PA3)
        "lsls %[tmp], #3\n\t"             // gap at the start (PA3-0)
        "eors %[tmp], %[xor]\n\t"

        // Store 16-cycle bitplane (of which the upper bits weren't cleared).
        "str %[tmp], [%[bitplane], #8]\n\t"

        : [tmp]"=&r"(tmp),
          [led0]"+&r"(led0),
          [led1]"+&r"(led1),
          [led2]"+&r"(led2)
        : [bitplane]"r"(bitplane),
          [N8]"r"(N8),
          [xor]"r"(xor)
    );
}

static void bitbang_update_bitplane_4(uint32_t led0, uint32_t led1, uint32_t led2, uint32_t *bitplane) {
    uint32_t tmp;
    uint32_t N8 = 8;
    uint32_t xor = 0b1000011111111111;

    __asm__ __volatile__(
        // 1 cycle bitplane
        "lsls %[tmp], #7\n\t"             // gap at the start
        "lsrs %[led2], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "lsrs %[led1], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A7/PA5)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A8/PA4)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A9/PA3)
        "lsrs %[led0], #4\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A10/PA2)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A11/PA1)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // 2 cycle bitplane
        "lsls %[tmp], #7\n\t"             // gap at the start
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A7/PA5)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A8/PA4)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A9/PA3)
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A10/PA2)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A11/PA1)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // Store 1-cycle and 2-cycle bitplanes.
        "str %[tmp], [%[bitplane]]\n\t"

        // Not clearing %[tmp] here since we will be shifting all 32 bits out.

        // 4 cycle bitplane
        "lsls %[tmp], #7\n\t"             // gap at the start
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A7/PA5)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A8/PA4)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A9/PA3)
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A10/PA2)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A11/PA1)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // 8 cycle bitplane
        "lsls %[tmp], #7\n\t"             // gap at the start
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A7/PA5)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A8/PA4)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A9/PA3)
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A10/PA2)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A11/PA1)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // Store 4-cycle and 8-cycle bitplanes.
        "str %[tmp], [%[bitplane], #4]\n\t"

        // 16 cycle bitplane
        "lsls %[tmp], #7\n\t"             // gap at the start
        "rors %[led2], %[N8]\n\t"
        "lsrs %[led2], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 red   (A4/PA8)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 green (A5/PA7)
        "rors %[led2], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED2 blue  (A6/PA6)
        "rors %[led1], %[N8]\n\t"
        "lsrs %[led1], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 red   (A7/PA5)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 green (A8/PA4)
        "rors %[led1], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED1 blue  (A9/PA3)
        "rors %[led0], %[N8]\n\t"
        "lsrs %[led0], #9\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 red   (A10/PA2)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 green (A11/PA1)
        "rors %[led0], %[N8]\n\t"
        "adcs %[tmp], %[tmp], %[tmp]\n\t" // LED0 blue  (A12/PA0)
        "eors %[tmp], %[xor]\n\t"

        // Store 16-cycle bitplane (of which the upper bits weren't cleared).
        "str %[tmp], [%[bitplane], #8]\n\t"

        : [tmp]"=&r"(tmp),
          [led0]"+&r"(led0),
          [led1]"+&r"(led1),
          [led2]"+&r"(led2)
        : [bitplane]"r"(bitplane),
          [N8]"r"(N8),
          [xor]"r"(xor)
    );
}

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

static void bitbang_update_bitplane(uint32_t a, uint32_t b, uint32_t c, uint32_t d, uint32_t *bitplane) {
    uint32_t tmp;
    uint32_t N8 = 8;

    __asm__ __volatile__(
        "mvns %[a], %[a]\n\t"
        "mvns %[b], %[b]\n\t"
        "mvns %[c], %[c]\n\t"
        "mvns %[d], %[d]\n\t"

        // Not clearing %[tmp]: the 1 and 2 cycle bitplanes shift all 32 bits out.
        BB_PLANE(BB_FIRST) // 1 cycle bitplane
        BB_PLANE(BB_NEXT)  // 2 cycle bitplane
        "str %[tmp], [%[bitplane]]\n\t"
        BB_PLANE(BB_NEXT)  // 4 cycle bitplane
        BB_PLANE(BB_NEXT)  // 8 cycle bitplane
        "str %[tmp], [%[bitplane], #4]\n\t"
        BB_PLANE(BB_NEXT)  // 16 cycle bitplane (upper bits are the 8 cycle one)
        "str %[tmp], [%[bitplane], #8]\n\t"

        : [tmp]"=&r"(tmp),
          [a]"+&r"(a),
          [b]"+&r"(b),
          [c]"+&r"(c),
          [d]"+&r"(d)
        : [bitplane]"r"(bitplane),
          [N8]"r"(N8)
    );
}

// This is one giant assembly function that will update all LEDs.
static void bitbang_show_leds(volatile uint32_t *bitplanes, volatile void *gpio) {
    uint32_t v1, v2;
    uint32_t mask = 0b1000011111111111;

    // Using the GPIO registers directly with memory offsets to avoid using one
    // more register:
    // OTYPER: [%[gpio], #0x4]
    // ODR:    [%[gpio], #0x14]

    __asm__ __volatile__(
        // prepare: make A4/PA8 bitmask for OTYPER
        "movs %[v1], #1\n\t"
        "lsls %[v1], %[v1], #8\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"

        // set OTYPER to clear only A4/PA8
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 0, 12, 24
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
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
        "movs %[v1], #1\n\t"                 // [3] make A5/PA7 bitmask for OTYPER
        "lsls %[v1], %[v1], #7\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "nop\n\t"                            // [9] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // set OTYPER to clear only A5/PA7
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 1, 13, 25
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
        "movs %[v1], #1\n\t"                 // [3] make A6/PA6 bitmask for OTYPER
        "lsls %[v1], %[v1], #6\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "nop\n\t"                            // [9] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // set OTYPER to clear only A6/PA6
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 2, 14, 26
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
        "movs %[v1], #1\n\t"                 // [3] make A7/PA5 bitmask for OTYPER
        "lsls %[v1], %[v1], #5\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "nop\n\t"                            // [9] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // set OTYPER to clear only A7/PA5
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 3, 15, 27
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
        "movs %[v1], #1\n\t"                 // [3] make A8/PA4 bitmask for OTYPER
        "lsls %[v1], %[v1], #4\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "nop\n\t"                            // [9] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // set OTYPER to clear only A8/PA4
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 4, 16, 28
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
        "movs %[v1], #1\n\t"                 // [3] make A9/PA3 bitmask for OTYPER
        "lsls %[v1], %[v1], #3\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "nop\n\t"                            // [9] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // set OTYPER to clear only A9/PA3
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 5, 17, 29
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
        "movs %[v1], #1\n\t"                 // [3] make A10/PA2 bitmask for OTYPER
        "lsls %[v1], %[v1], #2\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "nop\n\t"                            // [9] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // set OTYPER to clear only A10/PA2
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 6, 18, 30
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
        "movs %[v1], #1\n\t"                 // [3] make A11/PA1 bitmask for OTYPER
        "lsls %[v1], %[v1], #1\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "nop\n\t"                            // [9] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // set OTYPER to clear only A11/PA1
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 7, 19, 31
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
        "movs %[v1], #1\n\t"                 // [3] make A12/PA0 bitmask for OTYPER
        "lsls %[v1], %[v1], #0\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "nop\n\t"                            // [9] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // set OTYPER to clear only A12/PA0
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 8, 20, 32
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
        "movs %[v1], #1\n\t"                 // [3] make A1/PA15 bitmask for OTYPER
        "lsls %[v1], %[v1], #15\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "nop\n\t"                            // [9] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // set OTYPER to clear only A1/PA15
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 9, 21, 33
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
        "movs %[v1], #1\n\t"                 // [3] make A2/PA10 bitmask for OTYPER
        "lsls %[v1], %[v1], #10\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "nop\n\t"                            // [9] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // set OTYPER to clear only A2/PA10
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 10, 22, 34
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
        "movs %[v1], #1\n\t"                 // [3] make A3/PA9 bitmask for OTYPER
        "lsls %[v1], %[v1], #9\n\t"
        "eors %[v1], %[v1], %[mask]\n\t"
        "adds %[bitplanes], #12\n\t"         // [1] increment bitplanes for next 3 words
        "ldr  %[v2], [%[bitplanes], #4]\n\t" // [2] load 8 and 4 cycle bitplanes
        "nop\n\t"                            // [9] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // set OTYPER to clear only A3/PA9
        "strh %[v1], [%[gpio], #0x4]\n\t"

        // Update LED 11, 23, 35
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
        "nop\n\t"                            // [15] nop
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "nop\n\t"
        "strh %[mask], [%[gpio], #0x14]\n\t" // [1] ** end 16 cycle bitplane

        // restore: set OTYPER back to the mask
        "strh %[mask], [%[gpio], #0x4]\n\t"
        : [v1]"=&r"(v1),
          [v2]"=&r"(v2),
          [bitplanes]"+&r"(bitplanes)
        : [gpio]"r"(gpio),
          [mask]"r"(mask)
    );
}
