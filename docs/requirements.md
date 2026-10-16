# Requirements matrix — Project 16 (Industrial Edge)

| Requirement | Status | Evidence |
|---|---|---|
| Industrial protocol ingestion (Modbus TCP) | VERIFIED | from-scratch client/server; tests; e2e |
| PLC simulator | VERIFIED | `internal/plcsim`, `cmd/plcsim` |
| Store-and-forward with durability | VERIFIED | crash-at-every-byte test; e2e SIGKILL with no gaps |
| Uplink to cloud (MQTT) | VERIFIED | from-scratch MQTT 3.1.1 against mosquitto; QoS 1 |
| No data loss across broker outages | VERIFIED | e2e: every seq delivered |
| Edge analytics: compression with error bound | VERIFIED | property test; benchmark max error ≤ deviation |
| Anomaly detection | VERIFIED | EWMA tests; spikes detected in e2e |
| Alarm management (ISA-18.2 states, hysteresis, delays) | VERIFIED | state machine tests; ack API |
| Observability (metrics, health) | VERIFIED | `/metrics`, `/healthz`; asserted in e2e |
| Throughput / latency measurement | VERIFIED | 1,000 tags at 10 Hz, 10k records/s, p99 127 ms |
| Configuration validation | VERIFIED | tests |
| Security (TLS, authentication) | VERIFIED | MQTT over TLS + client certificates + password, plaintext refusal; mosquitto tests and e2e (`docs/security-deployment.md`, `docs/evidence/2026-10-14/go_tests.log`, `e2e.log`) |
| OPC UA, Modbus RTU, other protocols | NOT IMPLEMENTED | — |
| Docker / deployment | VERIFIED (local) | Dockerfile + compose (gateway, TLS mosquitto, plcsim); smoke test passed here (`docs/evidence/2026-10-14/docker_smoke.log`). Not deployed anywhere. |
| CI (before 2026-10-14 changes) | VERIFIED | `.github/workflows/industrial-edge.yml` green on GitHub Actions (run 38516430120), including e2e with mosquitto |
| TinyML fault classifier (synthetic data) | VERIFIED (emulated) | INT8 MLP, 96.73 % on held-out-speed synthetic test set; float 96.70 % (`docs/tinyml.md`, `tinyml_pipeline.log`) |
| C INT8 kernel bit-exact with Python reference | VERIFIED | all 3,000 test windows, host gcc and QEMU Cortex-M4/M3 (`tinyml_pipeline.log`, `tinyml_ci_mode.log`) |
| Cortex-M footprint and speed | VERIFIED (QEMU estimate only) | ~11 KB flash, ~10.5 KB RAM; ≈ 490 k instructions/window from SysTick under `-icount`. No real-hardware cycles. |
| Real-board latency | BLOCKED (needs hardware) | script and steps in `docs/OWNER_ACTIONS.md`; board image runs bit-exact on QEMU netduinoplus2 |
| Edge inference in the gateway (Go port, bit-exact) | VERIFIED | `TestBitExactWholeTestSet`, gateway inference tests, TLS e2e, Docker smoke |
| Predictive-maintenance evaluation vs EWMA | VERIFIED (simulated) | `docs/predictive-maintenance.md`, `bench/results/2026-10-14/pdm.json` |
| Drift detection (PSI, Page-Hinkley) with alerts | VERIFIED (simulated drift) | `internal/drift` tests, gateway drift test |
| Real machine data | NOT IMPLEMENTED | only synthetic data; see `docs/OWNER_ACTIONS.md` |
| CI (test + tinyml + docker jobs) | VERIFIED | GitHub Actions run 38705880336 green on commit 8d1b77a: Go tests with race detector, e2e x2, host C + QEMU Cortex-M4/M3 bit-exactness, Docker smoke test. The previous run (38705318312) failed in the docker job on a password-file permission; fixed. |
