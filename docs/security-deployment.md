# Secure telemetry and Docker deployment

## MQTT over TLS (`internal/mqtt`, `internal/gateway/security.go`)

* The MQTT client can dial TLS (TLS 1.2 minimum, server certificate verified against `mqtt.tls.ca_file`, host name checked), present a client certificate (`cert_file`/`key_file`) and send a user name and password (`username` + `password_file` or `EDGE_MQTT_PASSWORD`).
* Refusals built into the gateway configuration:
  * `require_tls: true` without a `tls` section → the gateway refuses to start ("refusing plaintext MQTT");
  * a user name without TLS → refused (the password would travel in clear text) unless `allow_plaintext_auth` is set;
  * `tls` without `ca_file` → refused (no unverified TLS);
  * missing or unreadable certificate files → start-up error.
* Tests against a real mosquitto 2.0.18 with a TLS-only listener, `require_certificate true` and a password file (certificates generated per test in Go): a client with a valid certificate and password publishes and receives at QoS 1. Refused: wrong password (CONNACK 5), no credentials (CONNACK 5), no client certificate, client certificate from an untrusted CA, server certificate from an untrusted CA (x509 error), plaintext client on the TLS port. The e2e test `TestSecureUplinkWithEdgeInference` runs the built gateway over TLS + certificate + password and receives its predictions and fault alarm.
* Not done: certificate rotation without restart, CRL/OCSP checks, broker ACLs per topic, MQTT 5, a hardware key store.

## Docker

* `Dockerfile`: multi-stage, static Go binaries on `alpine:3.20`, non-root user, `/data` volume for the queue, health check on `/healthz`.
* `docker-compose.yml`: `mosquitto` (TLS-only, client certificates + password, not published to the host), `plcsim` (tank + hoist with a simulated outer-race fault), `edgegw` (config `deploy/gateway.docker.json`, `require_tls: true`).
* `deploy/certs/gen-certs.sh` creates a demo CA, broker and client certificates and the password file with `openssl` and `mosquitto_passwd` (into `deploy/certs/out`, git-ignored).
* `deploy/smoke.sh` generates certificates, builds and starts the stack, waits for `/healthz`, subscribes over TLS for 20 s, checks that samples, predictions and a fault alarm arrive, checks that a plaintext client and a TLS client without a certificate are refused, prints metrics and tears everything down.

Run here on 2026-10-14 (log `docs/evidence/2026-10-14/docker_smoke.log`): **passed**. In 20 s the subscriber received 244 samples, 39 predictions (all `bearing_outer`), 2 temperature anomalies and an UNACK `bearing_outer` fault alarm; both refusals happened; `edge_uplink_connected 1`, 42 inferences. Docker Hub rate-limited (HTTP 429) the first attempt; the second succeeded.

The demo keys are made world-readable inside the mounted directory so the containers' non-root users can read them. Do not do that in production.
