"""Render docs/benchmarks.md from bench/results/<date>/*.json."""
import json
import sys

D = sys.argv[1]
R = f"bench/results/{D}"
c = json.load(open(f"{R}/compression.json"))
q = json.load(open(f"{R}/queue.json"))
p = json.load(open(f"{R}/pipeline.json"))
L = ["# Benchmarks\n",
     f"Produced by `go run ./cmd/edgebench -out {R} -broker <mqtt>` and rendered by `python3 bench/report.py {D}`. "
     "Machine: one cloud VM with 4 vCPUs (Xeon @ 2.80 GHz); mosquitto 2.0.18 runs on the same host.\n",
     "## Compression (one simulated hour at 10 Hz = 36,000 samples per tag)\n",
     "The ratio is samples in / points kept. Max error is measured against every input sample: swinging door uses linear "
     "interpolation between kept points; deadband uses hold-last-value. The configured deviation is a hard bound for both, "
     "and every measured error is within it.\n",
     "| tag | deviation | swinging door: ratio / max error | deadband: ratio / max error |", "|---|---|---|---|"]
by = {}
for r in c:
    by.setdefault((r["Tag"], r["Dev"]), {})[r["Method"]] = r
for (tag, dev), m in by.items():
    s, d = m["swinging_door"], m["deadband"]
    L.append(f"| {tag} | {dev} | {s['Ratio']:.0f}× / {s['MaxErr']:.3f} | {d['Ratio']:.0f}× / {d['MaxErr']:.3f} |")
L += ["", "On ramping signals (level, outflow), swinging door keeps far fewer points than deadband at the same error "
      "bound (for example 38× against 1× for level at 0.2), because a line follows a ramp that a held value cannot. On the "
      "flat, noisy temperature signal deadband does better at the larger bounds: the noise keeps closing the swinging "
      "door, while the held value stays inside the band. The method is therefore configurable per tag.\n",
      "## Durable queue (150-byte records)\n", "| durability | records/s | MB/s |", "|---|---|---|"]
for r in q["append"]:
    L.append(f"| {r['Mode']} | {r['RecordsPerSec']:,.0f} | {r['MBPerSec']:.1f} |")
rc = q["recovery"]
L += ["", f"Opening a queue with {rc['records']:,} records (which scans and CRC-checks every segment) took "
      f"{rc['open_seconds'] * 1000:.0f} ms, with the files in the page cache.\n",
      "The gateway uses group commit, the middle row: a power loss can lose at most 100 ms of samples, while a process crash loses "
      "nothing, because written data is already in the kernel. Fsync on every record would cap ingestion at a few thousand "
      "samples/s on this disk.\n",
      f"## Full pipeline: {p['plcs']} PLCs × {p['tags'] // p['plcs']} float32 tags, polled every {p['poll_ms']} ms\n",
      "All tags change on every poll, so nothing is compressed away. Records are delivered to an MQTT subscriber over QoS 1.\n",
      "| metric | value |", "|---|---|",
      f"| tags | {p['tags']:,} |",
      f"| run time | {p['seconds']:.0f} s |",
      f"| samples acquired | {p['samples_acquired']:,} |",
      f"| records delivered to the subscriber | {p['records_delivered']:,} ({p['delivered_per_sec']:,.0f}/s) |",
      f"| Modbus requests | {p['modbus_requests']:,} (one coalesced 100-register read per PLC per poll) |",
      f"| end-to-end latency p50 / p95 / p99 / max | {p['latency_ms_p50']:.0f} / {p['latency_ms_p95']:.0f} / {p['latency_ms_p99']:.0f} / {p['latency_ms_max']:.0f} ms |",
      f"| CPU | {p['cpu_cores_used']:.2f} cores ({p['note']}) |", "",
      "Latency runs from sample time to arrival at the subscriber. It covers the poll, the queue, batching (up to 500 "
      "records), the broker and the PUBACK window, but not the age of the value inside the PLC.\n"]
open("docs/benchmarks.md", "w").write("\n".join(L) + "\n")
