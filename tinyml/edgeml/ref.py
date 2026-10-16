"""Integer reference implementation (the specification) of the edge
fault-classifier pipeline: fixed-point feature extraction and the INT8 MLP.

Every operation is integer arithmetic with explicitly defined rounding, so
the C kernel (tinyml/c), the Go port (internal/tinyml) and this file must
agree bit for bit. Arrays are batched: shape (B, ...), dtype int64.

Right shifts of negative numbers are arithmetic (floor) in all three
languages (numpy, Go, and GCC for ARM/x86).
"""
import math

import numpy as np

N = 1024
LOG2N = 10
FS = 5120
NBINS = N // 2
# spectrum bands (FFT bin edges, 5 Hz per bin)
BAND_EDGES = [1, 4, 8, 16, 32, 64, 128, 192, 256, 384, 512]
# orders of shaft speed in Q8: 1x, 2x, 3x, BPFO (3.58x), BPFI (5.42x)
ORD_1X, ORD_2X, ORD_3X, ORD_BPFO, ORD_BPFI = 256, 512, 768, 916, 1388
FEATURE_NAMES = (
    ["rms", "kurtosis", "crest", "ord1x", "ord2x", "ord3x"]
    + [f"band{BAND_EDGES[i]*5}_{BAND_EDGES[i+1]*5}Hz" for i in range(len(BAND_EDGES) - 1)]
    + ["env_bpfo", "env_bpfi", "env_1x", "env_total", "current", "rpm"]
)
NFEAT = len(FEATURE_NAMES)  # 22


def tables():
    """Q15 tables. Generated here once and exported verbatim to C and Go."""
    hann = [int(round(32767 * 0.5 * (1 - math.cos(2 * math.pi * n / N)))) for n in range(N)]
    cos_t = [int(round(32767 * math.cos(2 * math.pi * k / N))) for k in range(N // 2)]
    sin_t = [int(round(32767 * math.sin(2 * math.pi * k / N))) for k in range(N // 2)]
    bitrev = [int(format(i, f"0{LOG2N}b")[::-1], 2) for i in range(N)]
    return (np.array(hann, np.int64), np.array(cos_t, np.int64), np.array(sin_t, np.int64),
            np.array(bitrev, np.int64))


HANN, COS, SIN, BITREV = tables()


def log2q(u):
    """Q8 log2 approximation: 256*floor(log2 u) + next 8 mantissa bits
    (linear interpolation between powers of two). log2q(0) = 0. u: uint64."""
    u = np.asarray(u, dtype=np.uint64)
    k = np.zeros(u.shape, np.int64)
    v = u.copy()
    for sh in (32, 16, 8, 4, 2, 1):
        m = (v >> np.uint64(sh)) > 0
        k = np.where(m, k + sh, k)
        v = np.where(m, v >> np.uint64(sh), v)
    hi = np.where(k >= 8, u >> np.uint64(np.maximum(k - 8, 0)).astype(np.uint64), np.uint64(0))
    lo = np.where(k < 8, u << np.uint64(np.maximum(8 - k, 0)).astype(np.uint64), np.uint64(0))
    mant = (np.where(k >= 8, hi, lo) & np.uint64(255)).astype(np.int64)
    return np.where(u == 0, 0, k * 256 + mant).astype(np.int64)


def fft_power(xw):
    """Radix-2 DIT FFT, int32 data, Q15 twiddles, round-half-up after each
    complex multiply, no per-stage scaling. Returns |X[k]|^2 for k < N/2
    (uint64)."""
    B = xw.shape[0]
    re = xw[:, BITREV].astype(np.int64)
    im = np.zeros_like(re)
    size = 2
    while size <= N:
        half = size // 2
        step = N // size
        c = COS[np.arange(half) * step]
        s = SIN[np.arange(half) * step]
        re3 = re.reshape(B, N // size, size)
        im3 = im.reshape(B, N // size, size)
        ar, ai = re3[:, :, :half].copy(), im3[:, :, :half].copy()
        br, bi = re3[:, :, half:], im3[:, :, half:]
        tr = (br * c + bi * s + 16384) >> 15
        ti = (bi * c - br * s + 16384) >> 15
        re = np.concatenate([ar + tr, ar - tr], axis=2).reshape(B, N)
        im = np.concatenate([ai + ti, ai - ti], axis=2).reshape(B, N)
        size *= 2
    assert np.abs(re).max() < 2**31 and np.abs(im).max() < 2**31
    return (re[:, :NBINS] ** 2 + im[:, :NBINS] ** 2).astype(np.uint64)


def order_bin(rpm, oq8):
    den = 60 * FS * 256
    return (rpm.astype(np.int64) * oq8 * N + den // 2) // den


def order_energy(p, rpm, oq8):
    b = order_bin(rpm, oq8)
    out = np.zeros(p.shape[0], np.uint64)
    for off in (-1, 0, 1):
        idx = b + off
        ok = (idx >= 1) & (idx <= NBINS - 1)
        out += np.where(ok, p[np.arange(p.shape[0]), np.clip(idx, 0, NBINS - 1)], np.uint64(0))
    return out


def features(x, rpm, cur):
    """x: (B, 1024) int16 samples; rpm, cur: (B,) uint16. Returns (B, 22)
    int64 features in Q8 log2 units."""
    x = x.astype(np.int64)
    B = x.shape[0]
    m = x.sum(axis=1) >> LOG2N
    d = np.clip(x - m[:, None], -32768, 32767)
    e = (d * d).astype(np.uint64)
    s2 = e.sum(axis=1)
    s4 = ((e >> np.uint64(15)) ** 2).sum(axis=1)
    pk = np.abs(d).max(axis=1).astype(np.uint64)
    l2 = log2q(s2)
    f = np.zeros((B, NFEAT), np.int64)
    f[:, 0] = l2 - LOG2N * 256
    f[:, 1] = log2q(s4) - 2 * l2 + 40 * 256
    f[:, 2] = log2q(pk * pk) - l2 + LOG2N * 256
    p = fft_power((d * HANN + 16384) >> 15)
    f[:, 3] = log2q(order_energy(p, rpm, ORD_1X))
    f[:, 4] = log2q(order_energy(p, rpm, ORD_2X))
    f[:, 5] = log2q(order_energy(p, rpm, ORD_3X))
    for i in range(len(BAND_EDGES) - 1):
        f[:, 6 + i] = log2q(p[:, BAND_EDGES[i]:BAND_EDGES[i + 1]].sum(axis=1))
    # envelope: |first difference|, mean removed, Hann, FFT
    h = np.zeros_like(d)
    h[:, 1:] = d[:, 1:] - d[:, :-1]
    env = np.abs(h)
    env = env - (env.sum(axis=1) >> LOG2N)[:, None]
    pe = fft_power((env * HANN + 16384) >> 15)
    f[:, 16] = log2q(order_energy(pe, rpm, ORD_BPFO))
    f[:, 17] = log2q(order_energy(pe, rpm, ORD_BPFI))
    f[:, 18] = log2q(order_energy(pe, rpm, ORD_1X))
    f[:, 19] = log2q(pe[:, 1:NBINS].sum(axis=1))
    f[:, 20] = log2q(cur.astype(np.uint64))
    f[:, 21] = log2q(rpm.astype(np.uint64))
    return f


def quantize_input(f, lo, mul):
    """Per-feature affine map of Q8 features to int8:
    q = clamp(((F - lo) * mul + 2^15) >> 16, 0, 255) - 128."""
    q = ((f - lo) * mul + 32768) >> 16
    return np.clip(q, 0, 255) - 128


def requant(acc, m0, shift, zp):
    """out = zp + ((acc * m0 + 2^(30+shift)) >> (31+shift)), clamped to int8.
    m0 is a Q31 multiplier in [2^30, 2^31); shift >= 0."""
    sh = 31 + shift
    return np.clip(zp + ((acc * m0 + (np.int64(1) << (sh - 1))) >> sh), -128, 127)


def mlp_int8(q, layers):
    """layers: list of dicts with w (out,in) int8, b int32, m0, shift (per
    output channel), zp_in, zp_out. ReLU is the clamp at zp_out=-128 for
    hidden layers (their real range starts at 0). Returns int8 logits."""
    a = q.astype(np.int64)
    for L in layers:
        acc = (a - L["zp_in"]) @ L["w"].astype(np.int64).T + L["b"].astype(np.int64)
        a = requant(acc, L["m0"].astype(np.int64), L["shift"].astype(np.int64), L["zp_out"])
    return a


def argmax_first(logits):
    """Index of the first maximum (all implementations break ties this way)."""
    return np.argmax(logits, axis=1)


def load_bin(path):
    raw = np.fromfile(path, dtype=np.uint8).reshape(-1, 2064)
    label = raw[:, 0].astype(np.int64)
    rpm = raw[:, 2:4].copy().view("<u2")[:, 0]
    cur = raw[:, 4:6].copy().view("<u2")[:, 0]
    sev = raw[:, 8:12].copy().view("<f4")[:, 0]
    load = raw[:, 12:16].copy().view("<f4")[:, 0]
    x = raw[:, 16:].copy().view("<i2")
    return dict(label=label, rpm=rpm, cur=cur, sev=sev, load=load, x=x)
