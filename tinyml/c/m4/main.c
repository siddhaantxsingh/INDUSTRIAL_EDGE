/* Bare-metal test/benchmark driver for Cortex-M3/M4 under QEMU (mps2-an385
 * / mps2-an386) with semihosting:
 *   - reads test.bin (vibgen format), classifies every window, writes
 *     out.bin in the same format as the host driver (bit-exact check);
 *   - times each stage with SysTick (and reports DWT CYCCNT if it counts).
 * QEMU is NOT cycle-accurate. Run with -icount shift=0 so virtual time is
 * 1 ns per instruction; SysTick runs at the 25 MHz MPS2 system clock, so
 * one tick = 40 executed instructions. Reported numbers are therefore
 * instruction-count ESTIMATES, not real-hardware cycles. */
#include <stdint.h>
#include "semihost.h"
#include "../edge_ml.h"

#define SYST_CSR (*(volatile uint32_t *)0xE000E010)
#define SYST_RVR (*(volatile uint32_t *)0xE000E014)
#define SYST_CVR (*(volatile uint32_t *)0xE000E018)
#define DEMCR (*(volatile uint32_t *)0xE000EDFC)
#define DWT_CTRL (*(volatile uint32_t *)0xE0001000)
#define DWT_CYCCNT (*(volatile uint32_t *)0xE0001004)

/* SysTick is polled (no interrupt): COUNTFLAG is read and cleared by every
 * CSR read, and ticks() is called far more often than once per 2^24 ticks. */
static uint32_t wraps;
void systick_handler(void) {}
static uint64_t ticks(void) {
    if (SYST_CSR & (1u << 16)) wraps++;
    uint32_t v = SYST_CVR;
    if (SYST_CSR & (1u << 16)) { wraps++; v = SYST_CVR; }
    return (uint64_t)wraps * 0x1000000u + (0xFFFFFFu - v);
}

static char *utoa(uint64_t v, char *p) {
    char t[24]; int n = 0;
    do { t[n++] = (char)('0' + v % 10); v /= 10; } while (v);
    while (n) *p++ = t[--n];
    return p;
}
static void kv(const char *k, uint64_t v) {
    char buf[96]; char *p = buf;
    while (*k) *p++ = *k++;
    p = utoa(v, p); *p++ = '\n'; *p = 0;
    sh_puts(buf);
}

static unsigned char rec[2064];
static int16_t x[EDGE_N];

int main(void) {
    SYST_RVR = 0xFFFFFF; SYST_CVR = 0; SYST_CSR = 5; /* processor clock, no interrupt, enable */
    (void)ticks();
    DEMCR |= 1u << 24; DWT_CYCCNT = 0; DWT_CTRL |= 1;
    /* calibration: 2,000,000 instructions (subs + bne, 1,000,000 times) */
    uint64_t k0 = ticks();
    __asm__ volatile("ldr r0, =1000000\n1: subs r0, r0, #1\n bne 1b" ::: "r0", "cc");
    uint64_t k1 = ticks();
    kv("calib_2M_instr_ticks=", k1 - k0);
    int in = sh_open("test.bin", 1 /* rb */), out = sh_open("out.bin", 5 /* wb */);
    if (in < 0 || out < 0) { sh_puts("cannot open test.bin/out.bin\n"); sh_exit(1); }
    uint64_t tf = 0, tq = 0, tm = 0, tf_max = 0, tm_max = 0, tf_min = ~0ull, tm_min = ~0ull;
    uint32_t c0 = DWT_CYCCNT;
    uint32_t n = 0;
    while (sh_read(in, rec, sizeof rec) == 0) {
        uint16_t rpm = (uint16_t)(rec[2] | rec[3] << 8), cur = (uint16_t)(rec[4] | rec[5] << 8);
        for (int i = 0; i < EDGE_N; i++) x[i] = (int16_t)(rec[16 + 2 * i] | rec[17 + 2 * i] << 8);
        int32_t f[EDGE_NFEATURES];
        int8_t q[EDGE_NFEATURES], l[EDGE_NCLASSES];
        uint64_t t0 = ticks();
        edge_features(x, rpm, cur, f);
        uint64_t t1 = ticks();
        edge_quantize(f, q);
        uint64_t t2 = ticks();
        edge_mlp(q, l);
        uint64_t t3 = ticks();
        tf += t1 - t0; tq += t2 - t1; tm += t3 - t2;
        if (t1 - t0 > tf_max) tf_max = t1 - t0;
        if (t1 - t0 < tf_min) tf_min = t1 - t0;
        if (t3 - t2 > tm_max) tm_max = t3 - t2;
        if (t3 - t2 < tm_min) tm_min = t3 - t2;
        unsigned char ob[EDGE_NFEATURES * 4 + EDGE_NFEATURES + EDGE_NCLASSES];
        for (int i = 0; i < EDGE_NFEATURES; i++) {
            ob[4 * i] = (unsigned char)f[i]; ob[4 * i + 1] = (unsigned char)(f[i] >> 8);
            ob[4 * i + 2] = (unsigned char)(f[i] >> 16); ob[4 * i + 3] = (unsigned char)(f[i] >> 24);
            ob[EDGE_NFEATURES * 4 + i] = (unsigned char)q[i];
        }
        for (int i = 0; i < EDGE_NCLASSES; i++) ob[EDGE_NFEATURES * 5 + i] = (unsigned char)l[i];
        sh_write(out, ob, sizeof ob);
        n++;
    }
    uint32_t c1 = DWT_CYCCNT;
    sh_close(in); sh_close(out);
    kv("windows=", n);
    kv("systick_ticks_features_total=", tf);
    kv("systick_ticks_quantize_total=", tq);
    kv("systick_ticks_mlp_total=", tm);
    kv("systick_ticks_features_min=", tf_min);
    kv("systick_ticks_features_max=", tf_max);
    kv("systick_ticks_mlp_min=", tm_min);
    kv("systick_ticks_mlp_max=", tm_max);
    kv("dwt_cyccnt_delta=", (uint32_t)(c1 - c0));
    sh_exit(0);
    return 0;
}
