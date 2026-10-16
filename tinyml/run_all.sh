#!/usr/bin/env bash
# Full TinyML pipeline, from the project root:
#   synthetic data -> features + training + INT8 PTQ (Python) ->
#   C kernel on the host (bit-exact vs Python, whole test set) ->
#   bare-metal Cortex-M4 and Cortex-M3 builds in QEMU (bit-exact, SysTick
#   instruction-count estimates) -> footprint.
# Needs: go, python3 with tinyml/requirements.txt, gcc, gcc-arm-none-eabi,
# qemu-system-arm. Set PY to the python interpreter (default python3).
# SKIP_TRAIN=1 keeps the committed model (CI) and only checks bit-exactness.
set -euo pipefail
PY=${PY:-python3}
OUT=${OUT:-tinyml/c/m4/build}
go run ./cmd/vibgen -out tinyml/data
if [ "${SKIP_TRAIN:-0}" = 1 ]; then
  $PY - <<'PYEOF'
import sys, os, json, hashlib
sys.path.insert(0, "tinyml")
import numpy as np
from edgeml import ref
import importlib.util
spec = importlib.util.spec_from_file_location("train", "tinyml/train.py"); t = importlib.util.module_from_spec(spec); spec.loader.exec_module(t)
info = json.load(open("tinyml/model/model.json"))
te = ref.load_bin("tinyml/data/test.bin")
f = np.concatenate([ref.features(te["x"][i:i+1000], te["rpm"][i:i+1000], te["cur"][i:i+1000]) for i in range(0, len(te["x"]), 1000)])
q = ref.quantize_input(f, np.array(info["feat_lo"]), np.array(info["feat_mul"]))
# logits are checked through the C and Go kernels against the stored hash
open("tinyml/data/py_features.bin", "wb").write(b"".join(f[i].astype("<i4").tobytes() + q[i].astype("i1").tobytes() for i in range(len(f))))
print("python reference features computed for", len(f), "windows")
PYEOF
else
  $PY tinyml/train.py
fi
mkdir -p "$OUT"
gcc -O2 -std=c99 -Wall -Wextra -Werror -o "$OUT/edge_host" tinyml/c/host/main.c tinyml/c/edge_ml.c
"$OUT/edge_host" tinyml/data/test.bin "$OUT/host_out.bin"
want=$($PY -c 'import json;print(json.load(open("tinyml/model/model.json"))["test_expected_sha256"])')
got=$(sha256sum "$OUT/host_out.bin" | cut -d' ' -f1)
echo "host C sha256 $got"; echo "python sha256 $want"
[ "$got" = "$want" ] && echo "HOST C: BIT-EXACT with Python reference on the whole test set" || { echo "HOST C MISMATCH"; exit 1; }
if [ -f tinyml/data/test_expected.bin ]; then cmp tinyml/data/test_expected.bin "$OUT/host_out.bin"; fi
OUT_ABS=$(cd "$OUT" && pwd)
for cfg in "cortex-m4 mps2-an386 m4" "cortex-m3 mps2-an385 m3"; do
  set -- $cfg
  make -s -C tinyml/c/m4 CPU=$1 OUT="$OUT_ABS"
  ( cd tinyml/data && rm -f out.bin && timeout 900 qemu-system-arm -M $2 -cpu $1 -nographic -monitor none -serial none \
      -semihosting-config enable=on,target=native -icount shift=0 -kernel "$OUT_ABS/edge_$3.elf" )
  g=$(sha256sum tinyml/data/out.bin | cut -d' ' -f1)
  [ "$g" = "$want" ] && echo "QEMU $1 ($2): BIT-EXACT on the whole test set" || { echo "QEMU $1 MISMATCH $g"; exit 1; }
  rm -f tinyml/data/out.bin
  arm-none-eabi-size "$OUT_ABS/edge_$3.elf"
  arm-none-eabi-size -A "$OUT_ABS/$3/edge_ml.o" | awk '/^\.text|^\.rodata|^\.bss|^\.data/ {s[substr($1,1,5)]+=$2} END {for (k in s) print "edge_ml.o", k, s[k]}'
  cat "$OUT_ABS/$3/edge_ml.su"
done
