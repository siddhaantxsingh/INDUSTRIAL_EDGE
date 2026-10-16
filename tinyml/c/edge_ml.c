/* See edge_ml.h. The integer semantics are defined in tinyml/edgeml/ref.py. */
#include "edge_ml.h"
#include "model_data.h"

#define LOG2N 10
#define NBINS (EDGE_N / 2)

static int16_t d_buf[EDGE_N];
static int32_t re_buf[EDGE_N];
static int32_t im_buf[EDGE_N];

static const uint16_t band_edges[11] = {1, 4, 8, 16, 32, 64, 128, 192, 256, 384, 512};

static int32_t log2q(uint64_t u) {
    if (u == 0) return 0;
    int k = 63 - __builtin_clzll(u);
    uint32_t m = (k >= 8) ? (uint32_t)(u >> (k - 8)) & 255u : (uint32_t)(u << (8 - k)) & 255u;
    return k * 256 + (int32_t)m;
}

/* radix-2 DIT on re_buf/im_buf, input already in bit-reversed order */
static void fft_core(void) {
    for (int size = 2; size <= EDGE_N; size <<= 1) {
        int half = size >> 1, step = EDGE_N / size;
        for (int start = 0; start < EDGE_N; start += size) {
            for (int j = 0; j < half; j++) {
                int32_t c = edge_cos[j * step], s = edge_sin[j * step];
                int a = start + j, b = a + half;
                int64_t br = re_buf[b], bi = im_buf[b];
                int32_t tr = (int32_t)((br * c + bi * s + 16384) >> 15);
                int32_t ti = (int32_t)((bi * c - br * s + 16384) >> 15);
                int32_t ar = re_buf[a], ai = im_buf[a];
                re_buf[a] = ar + tr; im_buf[a] = ai + ti;
                re_buf[b] = ar - tr; im_buf[b] = ai - ti;
            }
        }
    }
}

static uint64_t pw(int k) {
    int64_t r = re_buf[k], i = im_buf[k];
    return (uint64_t)(r * r) + (uint64_t)(i * i);
}

static uint64_t order_energy(uint16_t rpm, uint32_t oq8) {
    const uint64_t den = 60ull * 5120ull * 256ull;
    int64_t b = (int64_t)(((uint64_t)rpm * oq8 * EDGE_N + den / 2) / den);
    uint64_t e = 0;
    for (int64_t k = b - 1; k <= b + 1; k++)
        if (k >= 1 && k <= NBINS - 1) e += pw((int)k);
    return e;
}

void edge_features(const int16_t *x, uint16_t rpm, uint16_t cur, int32_t *f) {
    int32_t sum = 0;
    for (int n = 0; n < EDGE_N; n++) sum += x[n];
    int32_t m = sum >> LOG2N;
    uint64_t s2 = 0, s4 = 0;
    uint32_t pk = 0;
    for (int n = 0; n < EDGE_N; n++) {
        int32_t v = (int32_t)x[n] - m;
        if (v > 32767) v = 32767;
        if (v < -32768) v = -32768;
        d_buf[n] = (int16_t)v;
        uint32_t e = (uint32_t)(v * v);
        s2 += e;
        uint64_t e15 = e >> 15;
        s4 += e15 * e15;
        uint32_t a = (uint32_t)(v < 0 ? -v : v);
        if (a > pk) pk = a;
    }
    int32_t l2 = log2q(s2);
    f[0] = l2 - LOG2N * 256;
    f[1] = log2q(s4) - 2 * l2 + 40 * 256;
    f[2] = log2q((uint64_t)pk * pk) - l2 + LOG2N * 256;

    /* spectrum of the Hann-windowed signal */
    for (int i = 0; i < EDGE_N; i++) {
        int n = edge_bitrev[i];
        re_buf[i] = (int32_t)(((int64_t)d_buf[n] * edge_hann[n] + 16384) >> 15);
        im_buf[i] = 0;
    }
    fft_core();
    f[3] = log2q(order_energy(rpm, 256));
    f[4] = log2q(order_energy(rpm, 512));
    f[5] = log2q(order_energy(rpm, 768));
    for (int bnd = 0; bnd < 10; bnd++) {
        uint64_t e = 0;
        for (int k = band_edges[bnd]; k < band_edges[bnd + 1]; k++) e += pw(k);
        f[6 + bnd] = log2q(e);
    }

    /* envelope spectrum: |first difference|, mean removed, Hann */
    int32_t esum = 0;
    for (int n = 1; n < EDGE_N; n++) {
        int32_t h = (int32_t)d_buf[n] - d_buf[n - 1];
        esum += h < 0 ? -h : h;
    }
    int32_t em = esum >> LOG2N;
    for (int i = 0; i < EDGE_N; i++) {
        int n = edge_bitrev[i];
        int32_t h = n == 0 ? 0 : (int32_t)d_buf[n] - d_buf[n - 1];
        int32_t e = (h < 0 ? -h : h) - em;
        re_buf[i] = (int32_t)(((int64_t)e * edge_hann[n] + 16384) >> 15);
        im_buf[i] = 0;
    }
    fft_core();
    f[16] = log2q(order_energy(rpm, 916));
    f[17] = log2q(order_energy(rpm, 1388));
    f[18] = log2q(order_energy(rpm, 256));
    uint64_t et = 0;
    for (int k = 1; k < NBINS; k++) et += pw(k);
    f[19] = log2q(et);
    f[20] = log2q(cur);
    f[21] = log2q(rpm);
}

void edge_quantize(const int32_t *f, int8_t *q) {
    for (int i = 0; i < EDGE_NFEAT; i++) {
        int64_t v = (((int64_t)f[i] - edge_feat_lo[i]) * edge_feat_mul[i] + 32768) >> 16;
        if (v < 0) v = 0;
        if (v > 255) v = 255;
        q[i] = (int8_t)(v - 128);
    }
}

static int8_t requant(int32_t acc, int32_t m0, int sh, int32_t zp) {
    int s = 31 + sh;
    int64_t v = zp + (((int64_t)acc * m0 + ((int64_t)1 << (s - 1))) >> s);
    if (v < -128) v = -128;
    if (v > 127) v = 127;
    return (int8_t)v;
}

static void dense(const int8_t *in, int n_in, int32_t zp_in, const int8_t *w, const int32_t *b, const int32_t *m0,
                  const int8_t *sh, int32_t zp_out, int n_out, int8_t *out) {
    for (int o = 0; o < n_out; o++) {
        int32_t acc = b[o];
        const int8_t *wr = w + o * n_in;
        for (int i = 0; i < n_in; i++) acc += ((int32_t)in[i] - zp_in) * wr[i];
        out[o] = requant(acc, m0[o], sh[o], zp_out);
    }
}

void edge_mlp(const int8_t *q, int8_t *logits) {
    int8_t h1[EDGE_H1], h2[EDGE_H2];
    dense(q, EDGE_NFEAT, EDGE_ZPIN0, edge_w0, edge_b0, edge_m0, edge_s0, EDGE_ZPOUT0, EDGE_H1, h1);
    dense(h1, EDGE_H1, EDGE_ZPIN1, edge_w1, edge_b1, edge_m1, edge_s1, EDGE_ZPOUT1, EDGE_H2, h2);
    dense(h2, EDGE_H2, EDGE_ZPIN2, edge_w2, edge_b2, edge_m2, edge_s2, EDGE_ZPOUT2, EDGE_NCLASS, logits);
}

int edge_argmax(const int8_t *l) {
    int best = 0;
    for (int i = 1; i < EDGE_NCLASS; i++)
        if (l[i] > l[best]) best = i;
    return best;
}

int edge_classify(const int16_t *x, uint16_t rpm, uint16_t cur, int32_t *feat, int8_t *q, int8_t *logits) {
    int32_t f[EDGE_NFEAT];
    int8_t qq[EDGE_NFEAT], l[EDGE_NCLASS];
    edge_features(x, rpm, cur, f);
    edge_quantize(f, qq);
    edge_mlp(qq, l);
    for (int i = 0; i < EDGE_NFEAT; i++) {
        if (feat) feat[i] = f[i];
        if (q) q[i] = qq[i];
    }
    if (logits)
        for (int i = 0; i < EDGE_NCLASS; i++) logits[i] = l[i];
    return edge_argmax(l);
}
