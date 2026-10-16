# Industrial Edge Gateway

This is an industrial IoT edge gateway in Go, using only the standard library. It:

1. polls PLCs over **Modbus TCP** (implemented from the spec);
2. runs **edge analytics**:
   - swinging-door compression with a proven error bound;
   - EWMA anomaly detection;
   - ISA-18.2 alarms;
3. buffers everything in a **crash-safe, disk-backed queue**;
4. forwards it over **MQTT 3.1.1 at QoS 1** (implemented from the spec), over **TLS with client certificates and passwords**, committing data only once the broker acknowledges it;
5. runs an **INT8 fault classifier** on vibration windows read over Modbus (TinyML, trained on synthetic data), raising ISA-18.2 fault alarms and **drift** alerts.

The same classifier is a dependency-free C kernel for **ARM Cortex-M**, bit-exact with the Python reference and the Go port, tested in QEMU (Cortex-M4/M3).

It loses nothing across broker outages and gateway crashes. A PLC simulator is included.

**Status:**
- **Done:** verified locally with a real broker (mosquitto) and a simulated plant.
- **Done (2026-10-14):** TinyML classifier, gateway inference, predictive-maintenance evaluation, drift detection, MQTT/TLS, Docker smoke test — all on simulated data or emulated hardware.
- **Not done:**
  - no real machine data, no real microcontroller measurement (steps in [OWNER_ACTIONS](docs/OWNER_ACTIONS.md));
  - Modbus TCP only;
  - not deployed.

See [limitations](docs/limitations.md).

## Results ([docs/benchmarks.md](docs/benchmarks.md))

| | |
|---|---|
| Broker outage + `kill -9` of the gateway + restart | every record delivered (no gaps); 3–9 duplicates from in-flight batches |
| 1,000 tags at 10 Hz (20 PLCs) → MQTT | 10,000 records/s sustained; end-to-end latency p50 42 ms, p99 127 ms; 0.59 cores including the simulated plant |
| Swinging-door compression | 38–105× on ramping signals at ±0.2–0.5, with the error bound held exactly (property-tested) |
| Durable queue | 733 k appends/s with 100 ms group commit (5.3 k/s fsync-per-record); 1 M-record recovery scan in 63 ms |
| Read coalescing | 50 tags → 1 Modbus request per poll |

## Edge ML results (synthetic data; [tinyml](docs/tinyml.md), [predictive maintenance](docs/predictive-maintenance.md))

| | |
|---|---|
| Fault classifier (6 classes, held-out speed band, 3,000 windows) | INT8 96.73 % accuracy, float 96.70 %; MLP 22-32-16-6, 1,366 parameters |
| Bit-exactness | Python reference = C (host) = C on QEMU Cortex-M4 and M3 = Go, on every test window |
| Cortex-M4 footprint (kernel) | ~3.0 KB code + 8.1 KB constants (6 KB are FFT tables); 10.2 KB RAM buffers, < 300 B stack |
| Cortex-M4 work per window (QEMU, not cycle-accurate) | ≈ 490,000 instructions (FFTs ≈ 98 %); real-board cycles not measured |
| Run-to-failure (simulated, 80 machines) | model detects 80/80 before failure, median lead 11.2 days, 0.11 false alarms/machine-day; EWMA baseline detects 0/80 |
| Drift | PSI alerts on a 70 % sensor-gain loss and on unseen speeds; none on 1,500 in-distribution windows |
| Docker | gateway + TLS mosquitto + simulator; smoke test passed |

## Run it

```bash
go build ./cmd/edgegw ./cmd/plcsim
./plcsim -listen 127.0.0.1:5020 -spike-every 500 &
mosquitto -p 1883 &
./edgegw -config configs/example.json          # metrics at http://127.0.0.1:9100/metrics
mosquitto_sub -t 'plant1/gw1/data' -q 1         # JSON batches
curl -X POST 'http://127.0.0.1:9100/alarms/ack?dev=tank1&tag=level'
```

With the hoist simulator and TLS: `deploy/smoke.sh` (Docker). TinyML pipeline: `tinyml/run_all.sh`.

Tests: `go test ./...` (MQTT and e2e need `mosquitto` on PATH or at `/usr/sbin/mosquitto`). Benchmarks: `go run ./cmd/edgebench -out bench/results/$(date +%F) -broker 127.0.0.1:1883`, then `python3 bench/report.py <date>`.

## Documentation

- [Architecture and failure behaviour](docs/architecture.md)
- [Testing](docs/testing.md)
- [Benchmarks](docs/benchmarks.md)
- [Limitations](docs/limitations.md)
- [Requirements matrix](docs/requirements.md)
- [TinyML classifier](docs/tinyml.md), [predictive maintenance](docs/predictive-maintenance.md), [security and Docker](docs/security-deployment.md)
- [Owner actions](docs/OWNER_ACTIONS.md)

## Scorecard (2026-10-14)

| Score (0–10) | | Why not higher |
|---|---|---|
| Fresher-portfolio strength | 8 | Everything ML runs on synthetic data and emulated hardware; no real sensor or board. |
| Engineering maturity | 7 | Strong tests (bit-exact checks, crash tests, TLS refusals), but drift/alarm thresholds are untuned and one e2e test flaked under heavy machine load. |
| Deployment readiness | 4 | Docker smoke test only; no real PLCs, no fleet management, demo PKI, model false-alarm rate too high for production. |
| Completion of intended scope | 7 | TinyML, PdM evaluation, drift, TLS and Docker done; real-board latency and real data are owner-only; fleet cloud workflows not built. |
