"""Train, quantize and export the hoist fault classifier.

    go run ./cmd/vibgen -out tinyml/data          # synthetic dataset
    python tinyml/train.py                        # from the project root

Steps:
 1. integer features for every window (tinyml/edgeml/ref.py, the spec);
 2. per-feature int8 input quantization, range from the training set
    (0.5th..99.5th percentile);
 3. float MLP 22-32-16-6 (scikit-learn, ReLU, Adam), trained on the
    int8-quantized inputs scaled by 1/128;
 4. post-training INT8 quantization: per-output-channel symmetric weights,
    int32 biases, per-tensor asymmetric activations calibrated on 1,000
    training windows, fixed-point requantization (Q31 multiplier + shift);
 5. evaluation of float and int8 on the held-out-speed test set;
 6. export: tinyml/c/model_data.h, internal/tinyml/model_data.go,
    tinyml/model/model.json, and the expected integer outputs for the whole
    test set (tinyml/data/test_expected.bin) for the bit-exactness checks.
"""
import hashlib
import json
import math
import os
import sys
import time

import numpy as np
from sklearn.metrics import confusion_matrix, f1_score
from sklearn.neural_network import MLPClassifier

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from edgeml import ref  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
CLASSES = ["healthy", "imbalance", "misalignment", "bearing_outer", "bearing_inner", "overload"]
HIDDEN = (32, 16)
SEED = 0


def quant_multiplier(m):
    assert 0 < m < 1, m
    f, e = math.frexp(m)  # m = f * 2^e, f in [0.5, 1)
    m0 = int(round(f * (1 << 31)))
    if m0 == 1 << 31:
        m0 //= 2
        e += 1
    return m0, -e


def expected_bytes(f, q, logits):
    """Per window: 22 x int32 features, 22 x int8 inputs, 6 x int8 logits."""
    out = bytearray()
    for i in range(f.shape[0]):
        out += f[i].astype("<i4").tobytes() + q[i].astype("i1").tobytes() + logits[i].astype("i1").tobytes()
    return bytes(out)


def main():
    t0 = time.time()
    tr = ref.load_bin(os.path.join(HERE, "data", "train.bin"))
    te = ref.load_bin(os.path.join(HERE, "data", "test.bin"))
    ftr = np.concatenate([ref.features(tr["x"][i:i + 1000], tr["rpm"][i:i + 1000], tr["cur"][i:i + 1000])
                          for i in range(0, len(tr["x"]), 1000)])
    fte = np.concatenate([ref.features(te["x"][i:i + 1000], te["rpm"][i:i + 1000], te["cur"][i:i + 1000])
                          for i in range(0, len(te["x"]), 1000)])
    print(f"features: {time.time()-t0:.1f}s")

    lo = np.percentile(ftr, 0.5, axis=0).astype(np.int64)
    hi = np.percentile(ftr, 99.5, axis=0).astype(np.int64)
    hi = np.maximum(hi, lo + 1)
    mul = np.round(255 * 65536 / (hi - lo)).astype(np.int64)
    qtr = ref.quantize_input(ftr, lo, mul)
    qte = ref.quantize_input(fte, lo, mul)
    Xtr, Xte = qtr / 128.0, qte / 128.0
    ytr, yte = tr["label"], te["label"]

    clf = MLPClassifier(hidden_layer_sizes=HIDDEN, activation="relu", alpha=1e-3, max_iter=400,
                        early_stopping=True, validation_fraction=0.15, n_iter_no_change=25, random_state=SEED)
    clf.fit(Xtr, ytr)
    pf = clf.predict(Xte)
    acc_float = float((pf == yte).mean())
    print(f"float test accuracy {acc_float:.4f}  (train {clf.score(Xtr, ytr):.4f}, epochs {clf.n_iter_})")

    # ---- post-training quantization ----
    rng = np.random.default_rng(SEED)
    calib = Xtr[rng.choice(len(Xtr), 1000, replace=False)]
    acts = [calib]
    a = calib
    for li, (W, b) in enumerate(zip(clf.coefs_, clf.intercepts_)):
        a = a @ W + b
        if li < len(clf.coefs_) - 1:
            a = np.maximum(a, 0)
        acts.append(a)
    layers = []
    s_in, zp_in = 1 / 128, 0
    for li, (W, b) in enumerate(zip(clf.coefs_, clf.intercepts_)):
        Wt = W.T  # (out, in)
        sw = np.maximum(np.abs(Wt).max(axis=1), 1e-8) / 127
        wq = np.clip(np.round(Wt / sw[:, None]), -127, 127).astype(np.int64)
        bq = np.round(b / (s_in * sw)).astype(np.int64)
        out = acts[li + 1]
        last = li == len(clf.coefs_) - 1
        if not last:
            amax = float(out.max())
            s_out, zp_out = amax / 255, -128
        else:
            mn, mx = float(min(out.min(), 0)), float(max(out.max(), 0))
            s_out = (mx - mn) / 255
            zp_out = int(np.clip(round(-128 - mn / s_out), -128, 127))
        m0s, shifts = zip(*[quant_multiplier(s_in * s / s_out) for s in sw])
        layers.append(dict(w=wq, b=bq, m0=np.array(m0s, np.int64), shift=np.array(shifts, np.int64),
                           zp_in=zp_in, zp_out=zp_out, s_in=s_in, s_out=s_out, sw=sw))
        s_in, zp_in = s_out, zp_out
    logits = ref.mlp_int8(qte, layers)
    pq = ref.argmax_first(logits)
    acc_int8 = float((pq == yte).mean())
    agree = float((pq == pf).mean())
    print(f"int8 test accuracy {acc_int8:.4f}; float-int8 agreement {agree:.4f}")

    # ---- export ----
    os.makedirs(os.path.join(HERE, "model"), exist_ok=True)
    exp = expected_bytes(fte, qte, logits)
    with open(os.path.join(HERE, "data", "test_expected.bin"), "wb") as fh:
        fh.write(exp)
    sha = hashlib.sha256(exp).hexdigest()
    # reference histograms of the int8 inputs for PSI drift detection
    edges = np.linspace(-128, 128, 11)
    ref_hist = [(np.histogram(qtr[:, j], bins=edges)[0] / len(qtr)).round(6).tolist() for j in range(ref.NFEAT)]
    # reference confidence: dequantized softmax max on training windows
    ltr = ref.mlp_int8(qtr, layers)
    s_o, z_o = layers[-1]["s_out"], layers[-1]["zp_out"]
    z = (ltr - z_o) * s_o
    p = np.exp(z - z.max(1, keepdims=True))
    p /= p.sum(1, keepdims=True)
    conf = p.max(1)

    nparams = sum(L["w"].size + L["b"].size for L in layers)
    info = dict(
        dataset="SYNTHETIC (internal/hoistsim, cmd/vibgen -seed 1): train speeds 600-1000 and 1300-1750 rpm, "
                "test speeds 1000-1300 rpm (held out); severity 0.1-1.0",
        classes=CLASSES, features=ref.FEATURE_NAMES, hidden=list(HIDDEN), n_train=int(len(ytr)), n_test=int(len(yte)),
        params=int(nparams), weight_bytes=int(sum(L["w"].size for L in layers)),
        bias_bytes=int(4 * sum(L["b"].size for L in layers)),
        acc_float=acc_float, acc_int8=acc_int8, float_int8_agreement=agree,
        macro_f1_float=float(f1_score(yte, pf, average="macro")), macro_f1_int8=float(f1_score(yte, pq, average="macro")),
        confusion_int8=confusion_matrix(yte, pq).tolist(), confusion_float=confusion_matrix(yte, pf).tolist(),
        test_expected_sha256=sha, epochs=int(clf.n_iter_), sklearn_seed=SEED,
        logit_scale=s_o, logit_zp=z_o, train_conf_mean=float(conf.mean()), train_conf_std=float(conf.std()),
        feat_lo=lo.tolist(), feat_mul=mul.tolist(),
    )
    with open(os.path.join(HERE, "model", "model.json"), "w") as fh:
        json.dump(info, fh, indent=1)
    write_c(lo, mul, layers)
    write_go(lo, mul, layers, ref_hist, s_o, z_o, float(conf.mean()), float(conf.std()))
    print(json.dumps({k: info[k] for k in ("acc_float", "acc_int8", "float_int8_agreement", "macro_f1_int8",
                                           "params", "test_expected_sha256")}, indent=1))
    print("confusion int8 (rows true, cols predicted):")
    for row in info["confusion_int8"]:
        print("  ", row)
    print(f"total {time.time()-t0:.1f}s")


def arr(vals, per=16):
    vals = [str(int(v)) for v in vals]
    return ",\n".join("  " + ", ".join(vals[i:i + per]) for i in range(0, len(vals), per))


def write_c(lo, mul, layers):
    dims = [ref.NFEAT] + list(HIDDEN) + [len(CLASSES)]
    s = ["/* Generated by tinyml/train.py. Do not edit. SYNTHETIC-data model. */",
         "#ifndef EDGE_MODEL_DATA_H", "#define EDGE_MODEL_DATA_H", "#include <stdint.h>",
         f"#define EDGE_NFEAT {ref.NFEAT}", f"#define EDGE_H1 {HIDDEN[0]}", f"#define EDGE_H2 {HIDDEN[1]}",
         f"#define EDGE_NCLASS {len(CLASSES)}",
         f"static const int16_t edge_hann[{ref.N}] = {{\n{arr(ref.HANN)}\n}};",
         f"static const int16_t edge_cos[{ref.N//2}] = {{\n{arr(ref.COS)}\n}};",
         f"static const int16_t edge_sin[{ref.N//2}] = {{\n{arr(ref.SIN)}\n}};",
         f"static const uint16_t edge_bitrev[{ref.N}] = {{\n{arr(ref.BITREV)}\n}};",
         f"static const int32_t edge_feat_lo[{ref.NFEAT}] = {{\n{arr(lo)}\n}};",
         f"static const int32_t edge_feat_mul[{ref.NFEAT}] = {{\n{arr(mul)}\n}};"]
    for i, L in enumerate(layers):
        o, n = dims[i + 1], dims[i]
        s += [f"static const int8_t edge_w{i}[{o*n}] = {{\n{arr(L['w'].ravel())}\n}};",
              f"static const int32_t edge_b{i}[{o}] = {{\n{arr(L['b'])}\n}};",
              f"static const int32_t edge_m{i}[{o}] = {{\n{arr(L['m0'])}\n}};",
              f"static const int8_t edge_s{i}[{o}] = {{\n{arr(L['shift'])}\n}};",
              f"#define EDGE_ZPIN{i} ({L['zp_in']})", f"#define EDGE_ZPOUT{i} ({L['zp_out']})"]
    s.append("#endif")
    with open(os.path.join(HERE, "c", "model_data.h"), "w") as fh:
        fh.write("\n".join(s) + "\n")


def goarr(typ, vals, per=16):
    vals = [str(int(v)) for v in vals]
    return f"[]{typ}{{\n" + "".join("\t" + ", ".join(vals[i:i + per]) + ",\n" for i in range(0, len(vals), per)) + "}"


def write_go(lo, mul, layers, ref_hist, s_o, z_o, cmean, cstd):
    s = ["// Code generated by tinyml/train.py. DO NOT EDIT.", "// SYNTHETIC-data model; see tinyml/model/model.json.", "",
         "package tinyml", "",
         f"var hannQ15 = {goarr('int32', ref.HANN)}", f"var cosQ15 = {goarr('int32', ref.COS)}",
         f"var sinQ15 = {goarr('int32', ref.SIN)}", f"var bitrev = {goarr('int32', ref.BITREV)}",
         f"var featLo = {goarr('int64', lo)}", f"var featMul = {goarr('int64', mul)}", "",
         "var layers = []layer{"]
    for L in layers:
        o, n = L["w"].shape
        s.append(f"\t{{in: {n}, out: {o}, zpIn: {L['zp_in']}, zpOut: {L['zp_out']},\n"
                 f"\t\tw: {goarr('int8', L['w'].ravel())},\n\t\tb: {goarr('int64', L['b'])},\n"
                 f"\t\tm0: {goarr('int64', L['m0'])},\n\t\tshift: {goarr('int64', L['shift'])}}},")
    s.append("}")
    s.append(f"\n// LogitScale and LogitZP dequantize the int8 logits.\nconst (\n\tLogitScale = {s_o!r}\n\tLogitZP = {z_o}\n"
             f"\t// TrainConfMean/Std: max softmax probability on the training set.\n"
             f"\tTrainConfMean = {cmean!r}\n\tTrainConfStd = {cstd!r}\n)")
    s.append("\n// RefHist is the training distribution of each int8 input over 10 equal bins of [-128, 128).")
    s.append("var RefHist = [][]float64{")
    for h in ref_hist:
        s.append("\t{" + ", ".join(repr(v) for v in h) + "},")
    s.append("}")
    path = os.path.join(ROOT, "internal", "tinyml", "model_data.go")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as fh:
        fh.write("\n".join(s) + "\n")
    import shutil
    import subprocess
    gofmt = shutil.which("gofmt") or "/usr/local/go/bin/gofmt"
    if os.path.exists(gofmt):
        subprocess.run([gofmt, "-w", path], check=True)


if __name__ == "__main__":
    main()
