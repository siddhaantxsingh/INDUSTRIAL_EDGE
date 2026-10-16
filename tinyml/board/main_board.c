/* On-board check and timing for a real Cortex-M4 board (e.g. STM32
 * Nucleo-L476RG or -F446RE), run through a debugger with semihosting:
 *   - runs the classifier on 18 embedded test windows and compares the
 *     int8 logits with the Python reference (bit-exactness on hardware);
 *   - measures cycles with the DWT cycle counter (real cycles on hardware).
 * The core clock is whatever the reset state gives (16 MHz HSI on L4/F4
 * parts, 0 flash wait states); cycles are reported, convert with your clock. */
#include <stdint.h>
#include "../c/edge_ml.h"
#include "../c/m4/semihost.h"
#include "build/vectors.h"

#define DEMCR (*(volatile uint32_t *)0xE000EDFC)
#define DWT_CTRL (*(volatile uint32_t *)0xE0001000)
#define DWT_CYCCNT (*(volatile uint32_t *)0xE0001004)
#define DWT_LAR (*(volatile uint32_t *)0xE0001FB0)

void systick_handler(void) {}

static void kv(const char *k, uint32_t v) {
    char buf[80], t[12]; char *p = buf; int n = 0;
    while (*k) *p++ = *k++;
    do { t[n++] = (char)('0' + v % 10); v /= 10; } while (v);
    while (n) *p++ = t[--n];
    *p++ = '\n'; *p = 0;
    sh_puts(buf);
}

int main(void) {
    DEMCR |= 1u << 24;
    DWT_LAR = 0xC5ACCE55; /* unlock on parts that need it */
    DWT_CYCCNT = 0;
    DWT_CTRL |= 1;
    uint32_t mism = 0, fmax = 0, mmax = 0, fsum = 0, msum = 0;
    for (int i = 0; i < NVEC; i++) {
        int32_t f[EDGE_NFEATURES]; int8_t q[EDGE_NFEATURES], l[EDGE_NCLASSES];
        uint32_t c0 = DWT_CYCCNT;
        edge_features(vec_x[i], vec_rpm[i], vec_cur[i], f);
        uint32_t c1 = DWT_CYCCNT;
        edge_quantize(f, q);
        edge_mlp(q, l);
        uint32_t c2 = DWT_CYCCNT;
        for (int j = 0; j < EDGE_NCLASSES; j++) if (l[j] != vec_logits[i][j]) mism++;
        fsum += c1 - c0; msum += c2 - c1;
        if (c1 - c0 > fmax) fmax = c1 - c0;
        if (c2 - c1 > mmax) mmax = c2 - c1;
    }
    kv("windows=", NVEC);
    kv("logit_mismatches=", mism);
    kv("cycles_features_mean=", fsum / NVEC);
    kv("cycles_features_max=", fmax);
    kv("cycles_quant_mlp_mean=", msum / NVEC);
    kv("cycles_quant_mlp_max=", mmax);
    sh_puts(mism ? "RESULT: MISMATCH\n" : "RESULT: BIT-EXACT\n");
    sh_exit((int)mism); /* under OpenOCD this ends the semihosting session */
}
