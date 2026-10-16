#!/usr/bin/env bash
# Docker smoke test: certificates -> compose up -> gateway healthy ->
# predictions and a fault alarm arrive over MQTT/TLS -> plaintext and
# certificate-less clients are refused -> compose down.
set -euo pipefail
cd "$(dirname "$0")/.."
deploy/certs/gen-certs.sh mosquitto gw1
docker compose up -d --build
trap 'docker compose logs --no-color edgegw | tail -20; docker compose down -v' EXIT
for i in $(seq 1 60); do
  curl -fsS http://127.0.0.1:${EDGE_HTTP_PORT:-9100}/healthz >/dev/null 2>&1 && break; sleep 1
done
curl -fsS http://127.0.0.1:${EDGE_HTTP_PORT:-9100}/healthz
PW=$(cat deploy/certs/out/mqtt.password)
SUB="mosquitto_sub -h mosquitto -p 8883 --cafile /mosquitto/certs/ca.pem --cert /mosquitto/certs/client.pem \
  --key /mosquitto/certs/client.key -u gw1 -P $PW -t plant1/gw1/data -q 1"
echo "--- records received over TLS (20 s)"
docker compose exec -T mosquitto sh -c "timeout 20 $SUB" > deploy/smoke.out || true
python3 - <<'PY'
import json
kinds, preds, alarms = {}, {}, []
for line in open("deploy/smoke.out"):
    for r in json.loads(line):
        kinds[r["kind"]] = kinds.get(r["kind"], 0) + 1
        if r["kind"] == "prediction":
            preds[r["class"]] = preds.get(r["class"], 0) + 1
        if r["kind"] == "alarm" and r.get("tag") == "fault":
            alarms.append((r["class"], r["state"]))
print("kinds", kinds); print("predictions", preds); print("fault alarms", alarms)
assert kinds.get("sample", 0) > 0 and preds and alarms, "missing records"
PY
rm -f deploy/smoke.out
echo "--- plaintext client on the TLS port (must fail)"
if docker compose exec -T mosquitto timeout 5 mosquitto_pub -h mosquitto -p 8883 -u gw1 -P "$PW" -t x -m y; then
  echo "FAIL: plaintext accepted"; exit 1; else echo "refused as expected"; fi
echo "--- TLS client without a client certificate (must fail)"
if docker compose exec -T mosquitto timeout 5 mosquitto_pub -h mosquitto -p 8883 --cafile /mosquitto/certs/ca.pem -u gw1 -P "$PW" -t x -m y; then
  echo "FAIL: no client certificate accepted"; exit 1; else echo "refused as expected"; fi
echo "--- metrics"
curl -fsS http://127.0.0.1:${EDGE_HTTP_PORT:-9100}/metrics | grep -E "^edge_(inferences_total|uplink_connected|published_batches_total|drift_alerts_total)"
echo "SMOKE TEST PASSED"
