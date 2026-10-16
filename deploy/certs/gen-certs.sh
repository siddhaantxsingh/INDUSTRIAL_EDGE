#!/usr/bin/env bash
# Creates a private CA, a broker certificate and a gateway client
# certificate, plus the mosquitto password file, in deploy/certs/out.
# For a demo or a lab: in production, use your PKI and keep keys off disk images.
#   deploy/certs/gen-certs.sh [broker-hostname] [mqtt-user]
set -euo pipefail
HOST=${1:-mosquitto}
USER_NAME=${2:-gw1}
OUT=$(cd "$(dirname "$0")" && pwd)/out
mkdir -p "$OUT" && cd "$OUT"
days=365
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout ca.key -out ca.pem \
  -subj "/CN=industrial-edge demo CA" -days $days -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
issue() { # name cn ext
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$1.key" -out "$1.csr" -subj "/CN=$2" 2>/dev/null
  printf '%s\n' "$3" > "$1.ext"
  openssl x509 -req -in "$1.csr" -CA ca.pem -CAkey ca.key -CAcreateserial -out "$1.pem" -days $days -extfile "$1.ext" 2>/dev/null
  rm -f "$1.csr" "$1.ext"
}
issue server "$HOST" "subjectAltName=DNS:$HOST,DNS:localhost,IP:127.0.0.1
extendedKeyUsage=serverAuth
keyUsage=digitalSignature"
issue client "$USER_NAME" "extendedKeyUsage=clientAuth
keyUsage=digitalSignature"
# password: random unless MQTT_PASSWORD is set
PW=${MQTT_PASSWORD:-$(openssl rand -hex 16)}
printf '%s\n' "$PW" > mqtt.password
if command -v mosquitto_passwd >/dev/null; then
  rm -f passwd; mosquitto_passwd -b -c passwd "$USER_NAME" "$PW"
else
  rm -f passwd; docker run --rm --user "$(id -u):$(id -g)" -v "$OUT":/out eclipse-mosquitto:2.0.18 sh -c "mosquitto_passwd -b -c /out/passwd '$USER_NAME' '$PW'"
fi
# readable by the containers' non-root users (demo only)
chmod 644 ./*.pem ./*.key passwd mqtt.password
openssl verify -CAfile ca.pem server.pem client.pem
echo "certificates and password file in $OUT"
