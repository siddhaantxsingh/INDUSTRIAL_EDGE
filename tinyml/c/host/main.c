/* Host test driver: runs the C kernel over a vibgen .bin file and writes
 * the same byte stream as tinyml/train.py (per window: 22 x int32 features,
 * 22 x int8 inputs, 6 x int8 logits) for a bit-exact comparison. */
#include <stdio.h>
#include <string.h>
#include "../edge_ml.h"

int main(int argc, char **argv) {
    if (argc != 3) { fprintf(stderr, "usage: %s test.bin out.bin\n", argv[0]); return 2; }
    FILE *in = fopen(argv[1], "rb"), *out = fopen(argv[2], "wb");
    if (!in || !out) { perror("open"); return 1; }
    unsigned char rec[2064];
    int16_t x[EDGE_N];
    long n = 0;
    while (fread(rec, 1, sizeof rec, in) == sizeof rec) {
        uint16_t rpm = rec[2] | rec[3] << 8, cur = rec[4] | rec[5] << 8;
        for (int i = 0; i < EDGE_N; i++) x[i] = (int16_t)(rec[16 + 2 * i] | rec[17 + 2 * i] << 8);
        int32_t f[EDGE_NFEATURES];
        int8_t q[EDGE_NFEATURES], l[EDGE_NCLASSES];
        edge_classify(x, rpm, cur, f, q, l);
        for (int i = 0; i < EDGE_NFEATURES; i++) {
            unsigned char b[4] = {f[i] & 255, (f[i] >> 8) & 255, (f[i] >> 16) & 255, (f[i] >> 24) & 255};
            fwrite(b, 1, 4, out);
        }
        fwrite(q, 1, EDGE_NFEATURES, out);
        fwrite(l, 1, EDGE_NCLASSES, out);
        n++;
    }
    fclose(out);
    fprintf(stderr, "%ld windows\n", n);
    return 0;
}
