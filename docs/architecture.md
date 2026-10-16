# Architecture

```
 PLCs (Modbus TCP) ──► poller per device ──► decode ──► anomaly (EWMA z) ──► alarm FSM (ISA-18.2)
                       coalesced block reads              │                    │
                                                          ▼                    ▼
                                             compression (swinging door / deadband / none)
                                                          │ records (JSON)
                                                          ▼
                              durable queue: CRC-framed segments, group-commit fsync, ack offset
                                                          │
                                                          ▼
                     uplink: batches of ≤ 500 records ──► MQTT QoS 1 ──► broker ──► consumers (dedup on seq)
                                    commit ack offset on PUBACK
```

Everything uses only the Go standard library. Modbus TCP and MQTT 3.1.1 are implemented from their specifications.

Edge ML branch: a device with a `vibration` section is read every `period_ms` (seq, speed, current, 1,024 samples in 9 Modbus reads, seq again to reject torn windows) → fixed-point features + INT8 MLP (`internal/tinyml`) → `prediction` records, a `fault` pseudo-tag with an ISA-18.2 alarm (on-delay = `alarm_windows`), and `drift` records (PSI, Page-Hinkley) → same queue and uplink. The uplink can use TLS with client certificates and a password.

## Data path

* **Polling.** Each device has a goroutine that polls every `poll_ms`.
  * Tags are coalesced into the fewest block reads: same table, sorted addresses, gaps of ≤ 8 registers bridged, ≤ 125 registers (or 2,000 coils) per read. 50 float32 tags at consecutive addresses take one request.
  * A failed read marks its tags bad, emitting one `quality: bad` record per tag; the first good read marks them good again.
  * `/healthz` fails when a device has not answered for 5 poll periods plus its timeout.
* **Decoding.** Supported types are uint16, int16, uint32, int32, float32 (big-endian, with optional word swap) and bool (coils). Each tag has a linear scale and offset.
* **Analytics, per tag.**
  * The EWMA z-score is computed against statistics from *before* the sample, so a spike cannot mask itself. Every sample with |z| ≥ the threshold produces an `anomaly` record.
  * The high alarm uses on/off delays and a deadband (hysteresis) and follows the ISA-18.2 states NORMAL → UNACK → ACKED / RTN_UNACK. Acknowledgement is `POST /alarms/ack`.
  * Compression:
    * **Swinging door:** feasible-slope form, which guarantees the error bound; see below.
    * **Deadband:** hold-last-value.
    * Both have a max-gap heartbeat.
* **Queue.**
  * Every record is appended to a segmented log as `[len][crc32c][payload]`.
  * A syncer goroutine fsyncs every `sync_ms` (group commit).
  * The acknowledged offset is persisted by write, fsync, rename and directory fsync.
  * On start-up a torn or zero-filled tail of the last segment is truncated; corruption anywhere else is an error.
  * `max_bytes` caps disk use by deleting the oldest segments, counted in `edge_queue_dropped_total`, so acquisition never stops.
* **Uplink.**
  * Reads batches from the last *sent* offset and stamps each record with `seq` = its queue offset.
  * Publishes each batch as a JSON array on `<prefix>/data` at QoS 1.
  * Keeps up to `inflight` batches in flight and commits the queue offset as each batch's PUBACK arrives, in order.
  * On any connection error, in-flight batches are abandoned and the uplink reconnects with exponential back-off (100 ms to 5 s). It restarts from the committed offset.
  * Delivery is therefore **at least once**; consumers de-duplicate on `(gateway_id, seq)`.

## Swinging-door error bound

The textbook description archives the *previous raw sample* when the doors close. A property test with 200 random signals shows this can exceed the deviation. For example, seed 0 with dev 0.05 gave an error of 0.059. The reason is that the straight line to the previous raw sample can leave the band of an earlier sample.

This implementation instead tracks the interval of slopes that keep the line from the last archived point within ±dev of *every* sample since. When a new sample empties that interval, it archives the point at the previous sample's time on the middle feasible slope; that point is within dev of the true value. The reconstruction then provably stays within ±dev, and the property test checks it.

## Failure behaviour

| Failure | Behaviour | Tested by |
|---|---|---|
| Broker down | acquisition continues into the queue; `edge_uplink_connected 0`; the backlog drains after reconnect | e2e `TestNoLossAcrossBrokerOutageAndGatewayKill` |
| Gateway SIGKILL | written records survive in the page cache; on restart, in-flight batches are re-sent (duplicates, no gaps) | same e2e test |
| Power loss | at most `sync_ms` of samples lost, plus a torn tail, which is truncated | queue torn-tail test (cut at every byte) |
| PLC down / slow | per-request timeout; bad-quality records; health fails; recovers on reconnect | e2e `TestPLCOutageReportsBadThenGoodQuality`; Modbus timeout test |
| Disk full / budget | the oldest segments are dropped and counted | queue cap test |
| Malformed Modbus / MQTT frames | rejected before allocation; the connection is dropped | Modbus malformed-frame tests; MQTT length checks |
