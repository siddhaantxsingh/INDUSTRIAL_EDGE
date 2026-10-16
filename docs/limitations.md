# Limitations

* **Protocols:** Modbus TCP only (no Modbus RTU/serial, OPC UA, EtherNet/IP or S7). MQTT 3.1.1 at QoS 0/1 only. TLS, client certificates and passwords are supported (see `security-deployment.md`); there is no certificate rotation without restart, no CRL/OCSP, and no broker ACL set-up. MQTT 5 / Sparkplug B would be the next step.
* **Writes to PLCs** are implemented in the client but not exposed through the gateway API. There is no control path, by design.
* **At-least-once, not exactly-once:** consumers must de-duplicate on `seq`. Gateway-side state is not de-duplicated across restarts: swinging-door state and alarm state are in memory, so after a crash compression restarts from the next sample and alarms re-evaluate from NORMAL.
* **The disk cap drops whole segments.** The budget can be exceeded by up to one segment (`segment_bytes`), so `max_bytes` should be at least 2 × `segment_bytes`.
* **Single gateway process:** no clustering or hot standby.
* **Benchmarks are single-host** with a simulated plant; latency excludes the PLC's own scan time. Real PLCs are usually slower to answer than the simulator.
* **Docker** image and compose stack were built and smoke-tested on one machine only; no orchestration, no service unit, nothing deployed.
* **TinyML is synthetic and emulated.** The classifier was trained and tested only on simulated vibration data; real machines will differ (sensor mounting, other resonances, mixed faults). Cortex-M speed figures are QEMU instruction counts, not cycles; no real board was used.
* **Predictive-maintenance results are simulated.** The model's false-alarm rate (0.11 per machine-day on the simulator) is too high for production, and lead times are optimistic (see `predictive-maintenance.md`).
* **Vibration over Modbus** reads 1,024 registers in 9 requests per window; real sensors usually stream waveforms differently. Torn windows are detected with a sequence register and discarded.
* **Drift detectors are untuned** beyond the tests; PSI flags any operating condition outside the training range, including legitimate new speeds.
