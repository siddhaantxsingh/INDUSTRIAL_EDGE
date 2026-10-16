# Benchmarks

Produced by `go run ./cmd/edgebench -out bench/results/2026-10-14 -broker <mqtt>` and rendered by `python3 bench/report.py 2026-10-14`. Machine: one cloud VM with 4 vCPUs (Xeon @ 2.80 GHz); mosquitto 2.0.18 runs on the same host.

## Compression (one simulated hour at 10 Hz = 36,000 samples per tag)

The ratio is samples in / points kept. Max error is measured against every input sample: swinging door uses linear interpolation between kept points; deadband uses hold-last-value. The configured deviation is a hard bound for both, and every measured error is within it.

| tag | deviation | swinging door: ratio / max error | deadband: ratio / max error |
|---|---|---|---|
| level | 0.01 | 2× / 0.010 | 1× / 0.004 |
| level | 0.05 | 10× / 0.050 | 1× / 0.050 |
| level | 0.2 | 38× / 0.200 | 1× / 0.200 |
| level | 0.5 | 51× / 0.500 | 3× / 0.500 |
| temperature | 0.01 | 1× / 0.010 | 1× / 0.010 |
| temperature | 0.05 | 2× / 0.050 | 2× / 0.050 |
| temperature | 0.2 | 111× / 0.200 | 136× / 0.200 |
| temperature | 0.5 | 279× / 0.500 | 529× / 0.487 |
| outflow | 0.01 | 46× / 0.010 | 1× / 0.010 |
| outflow | 0.05 | 59× / 0.050 | 5× / 0.050 |
| outflow | 0.2 | 76× / 0.200 | 19× / 0.200 |
| outflow | 0.5 | 105× / 0.500 | 64× / 0.500 |

On ramping signals (level, outflow), swinging door keeps far fewer points than deadband at the same error bound (for example 38× against 1× for level at 0.2), because a line follows a ramp that a held value cannot. On the flat, noisy temperature signal deadband does better at the larger bounds: the noise keeps closing the swinging door, while the held value stays inside the band. The method is therefore configurable per tag.

## Durable queue (150-byte records)

| durability | records/s | MB/s |
|---|---|---|
| fsync every record | 5,347 | 0.7 |
| group commit (fsync every 100 ms) | 732,799 | 99.7 |
| no fsync (OS page cache) | 846,445 | 115.1 |

Opening a queue with 1,000,000 records (which scans and CRC-checks every segment) took 63 ms, with the files in the page cache.

The gateway uses group commit, the middle row: a power loss can lose at most 100 ms of samples, while a process crash loses nothing, because written data is already in the kernel. Fsync on every record would cap ingestion at a few thousand samples/s on this disk.

## Full pipeline: 20 PLCs × 50 float32 tags, polled every 100 ms

All tags change on every poll, so nothing is compressed away. Records are delivered to an MQTT subscriber over QoS 1.

| metric | value |
|---|---|
| tags | 1,000 |
| run time | 20 s |
| samples acquired | 201,000 |
| records delivered to the subscriber | 200,000 (10,000/s) |
| Modbus requests | 4,020 (one coalesced 100-register read per PLC per poll) |
| end-to-end latency p50 / p95 / p99 / max | 42 / 96 / 127 / 140 ms |
| CPU | 0.59 cores (process CPU includes the 20 simulated PLC servers, the value updater and the subscriber) |

Latency runs from sample time to arrival at the subscriber. It covers the poll, the queue, batching (up to 500 records), the broker and the PUBACK window, but not the age of the value inside the PLC.


## Edge ML (2026-10-14)

See [tinyml.md](tinyml.md) and [predictive-maintenance.md](predictive-maintenance.md). QEMU instruction counts are deterministic (identical in three runs: `tinyml_pipeline.log`, `tinyml_ci_mode.log`); the machine was shared with other jobs (load average 2.5 at the start of the pipeline run, 11.6 for the CI-mode run). Go inference: about 47 µs per window on the host.
