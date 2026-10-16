/* ARM semihosting (bkpt 0xAB), enough for file I/O and exit under QEMU. */
#ifndef SEMIHOST_H
#define SEMIHOST_H
#include <stdint.h>
static inline int sh_call(int op, void *arg) {
    register int r0 __asm__("r0") = op;
    register void *r1 __asm__("r1") = arg;
    __asm__ volatile("bkpt 0xAB" : "+r"(r0) : "r"(r1) : "memory");
    return r0;
}
static inline int sh_strlen(const char *s) { int n = 0; while (s[n]) n++; return n; }
static inline int sh_open(const char *path, int mode) { uint32_t a[3] = {(uint32_t)path, (uint32_t)mode, (uint32_t)sh_strlen(path)}; return sh_call(0x01, a); }
static inline int sh_close(int fd) { uint32_t a[1] = {(uint32_t)fd}; return sh_call(0x02, a); }
/* returns bytes NOT read/written */
static inline int sh_read(int fd, void *buf, int n) { uint32_t a[3] = {(uint32_t)fd, (uint32_t)buf, (uint32_t)n}; return sh_call(0x06, a); }
static inline int sh_write(int fd, const void *buf, int n) { uint32_t a[3] = {(uint32_t)fd, (uint32_t)buf, (uint32_t)n}; return sh_call(0x05, a); }
static inline void sh_puts(const char *s) { sh_call(0x04, (void *)s); }
static inline void sh_exit(int code) { uint32_t a[2] = {0x20026, (uint32_t)code}; sh_call(0x20, a); for (;;) {} }
#endif
