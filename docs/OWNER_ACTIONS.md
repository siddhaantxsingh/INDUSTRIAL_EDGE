# Owner actions

Only the owner can do these. Save every output under `docs/evidence/<date>/`.

## 1. Measure real latency on an STM32 Nucleo board (Cortex-M4)

Needed before any claim about on-device latency. Tested only in QEMU so far.

Hardware: a Nucleo-L476RG or Nucleo-F446RE (any Cortex-M4 with ≥ 64 KB RAM and ≥ 256 KB flash), USB cable. Software: `gcc-arm-none-eabi`, `openocd`, `gdb-multiarch`, Go, Python with `tinyml/requirements.txt`.

```bash
cd industrial-edge
go run ./cmd/vibgen -out tinyml/data            # synthetic test set
python3 tinyml/train.py                          # deterministic: rewrites the committed model unchanged (check git diff)
python3 tinyml/board/gen_vectors.py              # needs tinyml/data/test_expected.bin (written by train.py)
make -C tinyml/board                             # -> tinyml/board/build/edge_board.elf
# terminal 1 (use target/stm32f4x.cfg for the F446RE)
openocd -f interface/stlink.cfg -f target/stm32l4x.cfg
# terminal 2
gdb-multiarch tinyml/board/build/edge_board.elf \
  -ex 'target extended-remote :3333' -ex 'monitor reset halt' -ex 'monitor arm semihosting enable' \
  -ex load -ex continue
```

Expected in the OpenOCD terminal:

```
windows=18
logit_mismatches=0
cycles_features_mean=<N>
cycles_features_max=<N>
cycles_quant_mlp_mean=<N>
cycles_quant_mlp_max=<N>
RESULT: BIT-EXACT
```

The board runs at its reset clock (16 MHz HSI on L4/F4, no wait states). Latency = cycles / clock. QEMU's instruction count (about 490,000 per window) suggests roughly 0.5–0.75 M cycles; report what the board measures, with the board name and clock. Save the terminal output as `docs/evidence/<date>/nucleo_<board>.log` and update `docs/tinyml.md` and `docs/requirements.md`.

## 2. Real vibration data (optional, large effect on credibility)

The model has only seen simulated data. To test it on real faults, a public bearing dataset (e.g. CWRU or Paderborn) would need resampling to 5120 Hz and windows of 1024 samples, and the speed/current inputs would need values. Download hosts were not reachable from the build machine.

## 3. Check CI

Open the Actions tab for workflow `industrial-edge` on branch `ccr-a6e6c567-y4w7k8` and confirm the `test`, `tinyml` and `docker` jobs are green.
