# Testing

`go test ./...` runs everything; the MQTT and e2e tests start a real `mosquitto` and skip if it is not installed. `go test -race ./internal/...` is clean.

| Package | Tests |
|---|---|
| `internal/modbus` | 2,000 random frame round trips. Malformed MBAP headers (wrong protocol id, oversized or zero length) rejected before allocating. Client against server for every function code. Exception responses keep the connection, while dropped connections are re-dialled. Timeout against a slow server. The server drops garbage. |
| `internal/mqtt` | 500 QoS 1 messages through mosquitto, all PUBACKed and all received. Broker death is detected, and pending/new publishes fail. Remaining-length encoding at every boundary. |
| `internal/queue` | Append/read/ack across segments and reopen. A **crash at every byte offset** of the active segment recovers exactly the complete records and keeps appending. Zero-filled tail truncated. Corruption in an older segment is an error. The disk cap drops the oldest records and counts them exactly (dropped + backlog = appended). |
| `internal/analytics` | Swinging-door **error-bound property** over 200 random signals with steps and ramps, at three deviations. Max-gap heartbeat. Deadband. EWMA ignores noise (|z| < 5) and flags a 15σ spike. Alarm state machine: chatter suppression by the on-delay, the deadband, ack in UNACK and in RTN_UNACK. |
| `internal/gateway` | Decoding of every type, scale/offset and word swap. Read coalescing (bridging, limits, tags inside blocks). Config validation rejects unknown types, wrong tables, bad names, unknown compression and unknown fields. |
| `e2e` (built `edgegw` binary + simulated PLC + mosquitto) | ~1,000 records with a 2 s broker outage, a SIGKILL of the gateway and a restart, then a graceful stop *while the broker is down* (it returns during the drain window). **Every seq from 0 to the end is delivered** (duplicates allowed and counted: 3–9 per run); the uncompressed counter tag is monotonic in seq order; anomalies from injected spikes arrive. PLC outage: health check fails, every tag goes bad and then good again. Repeated 4× without a failure. |

| `internal/tinyml` | Go port bit-exact with the Python reference on all 3,000 test windows (SHA-256 of features, int8 inputs and logits); Q8 log2 cases. |
| `internal/drift` | PSI basics; no alert on 1,500 in-distribution windows; alerts on a 70 % sensor-gain loss and on 300–500 rpm; Page-Hinkley catches a confidence drop. |
| `internal/gateway` (inference) | Simulated hoist with an outer-race fault → `bearing_outer` predictions and an UNACK fault alarm; healthy hoist → no alarm; 25 % sensor gain → drift record. TLS config refusals. |
| `internal/mqtt` (TLS) | mosquitto with TLS + `require_certificate` + password file: valid client works; wrong password, no credentials, no client certificate, untrusted client CA, untrusted server CA and plaintext are refused. |
| `e2e` (TLS + inference) | Built gateway → TLS broker with certificate and password → subscriber receives predictions and the fault alarm; gateway with `require_tls` and no TLS refuses to start. |
| `tinyml/run_all.sh` | Host C and QEMU Cortex-M4/M3 outputs compared with the Python reference on the whole test set. |
| `deploy/smoke.sh` | Docker compose stack end to end over TLS. |

## Bug found by CI

CI on GitHub Actions caught a graceful-stop bug that local runs had missed. If shutdown began while the uplink was between connections, it exited without draining, leaving records queued until the next start (no data loss, but late delivery). The fix keeps the uplink reconnecting during the drain window. The e2e test now stops the gateway while the broker is down; it fails on the old code (53–55 records left in the queue) and passes with the fix.
