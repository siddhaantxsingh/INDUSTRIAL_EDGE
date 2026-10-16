/* Minimal Cortex-M3/M4 startup: vector table, .data copy, .bss zero. */
#include <stdint.h>
extern uint32_t _sidata, _sdata, _edata, _sbss, _ebss, _estack;
int main(void);
void systick_handler(void);
void reset_handler(void);
static void default_handler(void) { for (;;) {} }
__attribute__((section(".vectors"), used)) static void (*const vectors[16])(void) = {
    (void (*)(void))&_estack, reset_handler, default_handler, default_handler, default_handler, default_handler,
    default_handler, 0, 0, 0, 0, default_handler, default_handler, 0, default_handler, systick_handler,
};
void *memset(void *p, int c, unsigned long n) { unsigned char *d = p; while (n--) *d++ = (unsigned char)c; return p; }
void *memcpy(void *dst, const void *src, unsigned long n) {
    unsigned char *d = dst; const unsigned char *s = src; while (n--) *d++ = *s++; return dst;
}
void reset_handler(void) {
    uint32_t *s = &_sidata, *d = &_sdata;
    while (d < &_edata) *d++ = *s++;
    for (d = &_sbss; d < &_ebss;) *d++ = 0;
#if defined(__ARM_FP)
    /* enable the FPU (CP10/CP11) in case the compiler uses it */
    *(volatile uint32_t *)0xE000ED88 |= 0xFu << 20;
#endif
    main();
    for (;;) {}
}
