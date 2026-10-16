# TinyML fault classifier (synthetic data, emulated Cortex-M)

**Read this first.** The model is trained and tested on **synthetic data** from a physics-inspired simulator (`internal/hoistsim`). It has never seen a real machine. The Cortex-M numbers come from **QEMU, which is not cycle-accurate**. No real-hardware latency has been measured. This is a personal project, separate from any internship work.

## Pipeline

| Stage | What | Where |
|---|---|---|
| Data | Crane hoist motor + drive-end accelerometer, 1024 samples at 5120 Hz (0.2 s), speed and motor current from the drive. Classes: healthy, imbalance (1x ∝ speed²), misalignment (2x/1x/3x), bearing outer race (impacts at 3.58x ringing a 1.8–2.6 kHz resonance), bearing inner race (5.42x, modulated once per revolution), overload (1.05–1.5 × rated load). Noise, sensor gain (±30 %), machine baselines, gear mesh, random knocks (load handling) and all phases vary. Severity 0.1–1.0. | `internal/hoistsim`, `cmd/vibgen` |
| Split | 9,000 training windows at 600–1000 and 1300–1750 rpm; 3,000 test windows at **1000–1300 rpm (held-out speed band)**. | `hoistsim.Dataset` |
| Features | 22 integer features in Q8 log2 units: RMS, kurtosis, crest factor; energy at 1x/2x/3x shaft order (order tracking from the drive speed); 10 spectral bands (fixed-point radix-2 FFT, Q15 twiddles, Hann window); envelope spectrum (abs of first difference) at BPFO, BPFI, 1x and total; current; speed. | `tinyml/edgeml/ref.py` (spec) |
| Model | MLP 22-32-16-6, ReLU, scikit-learn (Adam), trained on the int8-quantized inputs. 1,366 parameters. | `tinyml/train.py` |
| Quantization | Post-training INT8: per-output-channel symmetric weights, int32 biases, per-tensor asymmetric activations calibrated on 1,000 training windows, fixed-point requantization (Q31 multiplier + shift, round half up). Inputs: per-feature affine map to int8 from training percentiles. | `tinyml/train.py` |
| Kernels | Python integer reference (the spec); dependency-free C99 kernel (own code, CMSIS-NN style, no CMSIS); Go port used by the gateway. | `tinyml/c`, `internal/tinyml` |

## Results (test set: 3,000 windows, held-out speed band)

| Metric | Value |
|---|---|
| Float accuracy | 96.70 % |
| INT8 accuracy | 96.73 % (macro F1 0.968) |
| Float vs INT8 | +0.03 points; the two agree on 99.77 % of windows |
| Main error | low-severity faults predicted healthy (38/500 inner-race, 13/500 imbalance; see `tinyml/model/model.json`) |
| Bit-exactness | Python reference = host C = QEMU Cortex-M4 = QEMU Cortex-M3 = Go, for all 3,000 windows (SHA-256 of every feature, int8 input and logit: `4b2a25fe…`) |

Evidence: `docs/evidence/2026-10-14/tinyml_pipeline.log` (full run with training), `tinyml_ci_mode.log` (committed model), Go test `TestBitExactWholeTestSet`.

## Footprint (arm-none-eabi-gcc 13.2.1, `-mcpu=cortex-m4 -mthumb -O2`, from `arm-none-eabi-size -A edge_ml.o`)

| Section | Bytes | Contents |
|---|---|---|
| .text | 3,044 | features, FFT, quantization, MLP |
| .rodata | 8,140 | weights 1,312 + biases/multipliers/shifts ~490 + input scaling 176; FFT/Hann/bit-reverse tables 6,144 |
| .bss | 10,240 | work buffers (2 × 4 KB FFT, 2 KB signal) |
| stack | < 300 | from `-fstack-usage` (deepest path classify → features → fft) |

So about **11 KB flash and about 10.5 KB RAM** for the kernel, excluding the 2 KB input window. The whole QEMU test image is 12.6 KB text + 14.4 KB bss.

## Speed in QEMU (estimates only)

QEMU runs with `-icount shift=0` (1 ns of virtual time per instruction); SysTick runs at the 25 MHz MPS2 clock, so one tick = 40 instructions. A calibration loop of 2,000,000 instructions read exactly 50,000 ticks. DWT CYCCNT is not modelled by QEMU (it read 0).

| Stage (per window) | QEMU Cortex-M4 | QEMU Cortex-M3 |
|---|---|---|
| Features (2 × 1024-point FFT etc.) | 11,997 ticks ≈ 480,000 instructions | 11,972 ticks ≈ 479,000 |
| Input quantization | ≈ 600 instructions | ≈ 600 |
| INT8 MLP (1,312 MACs) | 233–234 ticks ≈ 9,300 instructions | same |
| **Total** | **≈ 490,000 instructions** | ≈ 489,000 |

These are executed-instruction counts, deterministic across runs. They are **not cycles**. On a real Cortex-M4 the cycle count depends on CPI, flash wait states and memory. As an explicitly assumed range of 1.0–1.5 cycles per instruction, the estimate is about 0.5–0.75 M cycles, i.e. **about 6–9 ms at 80 MHz** or 31–46 ms at a 16 MHz reset clock. This is an estimate, not a measurement; `docs/OWNER_ACTIONS.md` has the script to measure real cycles with DWT on an STM32 Nucleo board. The MLP alone is about 2 % of the work; the FFTs dominate.

On the gateway host (Go port, Xeon 2.8 GHz, shared machine) one window takes about 47 µs (`go test -bench Classify ./internal/tinyml`).

## Board build

`tinyml/board` builds an STM32 image (flash 0x08000000) with 18 embedded test windows and DWT cycle counting. It builds here (48.8 KB text incl. 36 KB of test vectors) and runs bit-exact on QEMU's `netduinoplus2` (STM32F405 model), where DWT reads 0 (`docs/evidence/2026-10-14/board_build_qemu_netduinoplus2.log`). It has **not** run on a real board.

## Drift detection (`internal/drift`)

* **PSI** of each int8 model input over a sliding window (default 300 inferences, evaluated every 30) against the training histogram (10 bins, stored in `internal/tinyml/model_data.go`). Alert at max PSI ≥ 0.25, re-arm below 0.1.
* **Page-Hinkley** on the model confidence (softmax maximum), for a sustained drop.
* Tests (`go test ./internal/drift`): 1,500 windows drawn like the training data raise no alert (max PSI 0.07); a 70 % sensor-gain loss raises a PSI alert (PSI 0.42 on a band feature); speeds of 300–500 rpm (outside training) raise a PSI alert on `rpm`; a confidence drop 0.95 → 0.75 is caught by Page-Hinkley after 40 samples. In the gain-loss test the confidence test did **not** fire: the model stays confident on drifted inputs, which is why input PSI is needed.
* Note: the 1000–1300 rpm test band is itself outside the training speeds, so PSI on `rpm` would flag a fleet that runs only in that band. That is the intended behaviour (operating conditions not seen in training).
