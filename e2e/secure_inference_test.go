package e2e

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"industrial-edge/internal/gateway"
	"industrial-edge/internal/hoistsim"
	"industrial-edge/internal/mqtt"
	"industrial-edge/internal/plcsim"
	"industrial-edge/internal/testutil"
)

// The built gateway reads a simulated hoist vibration sensor over Modbus,
// classifies each window with the INT8 model and publishes predictions and
// the ISA-18.2 fault alarm over MQTT/TLS with a client certificate and a
// password; a TLS subscriber receives them.
func TestSecureUplinkWithEdgeInference(t *testing.T) {
	b := testutil.StartSecureBroker(t)
	p := b.PKI
	plcsim.HoistPeriod = 100 * time.Millisecond
	plant := plcsim.New(5, 0)
	plant.SetHoist(hoistsim.InnerRace, 0.8, 1)
	plc, err := plant.Listen("127.0.0.1:0", 50)
	if err != nil {
		t.Fatal(err)
	}
	defer plant.Close()

	dir := t.TempDir()
	pwFile := filepath.Join(dir, "mqtt.pw")
	os.WriteFile(pwFile, []byte(b.Password+"\n"), 0o600)
	cfg := map[string]any{
		"gateway_id": "gw1", "data_dir": filepath.Join(dir, "data"), "http": "",
		"mqtt": map[string]any{"addr": b.Addr, "topic_prefix": "plant1/gw1", "keepalive_s": 5, "require_tls": true,
			"tls":      map[string]any{"ca_file": p.CA, "cert_file": p.ClientCert, "key_file": p.ClientKey},
			"username": b.User, "password_file": pwFile},
		"devices": []any{map[string]any{"name": "hoist1", "addr": plc, "poll_ms": 200, "tags": []any{},
			"vibration": map[string]any{"seq_addr": plcsim.HoistSeqAddr, "rpm_addr": plcsim.HoistRPMAddr,
				"current_addr": plcsim.HoistCurAddr, "wave_addr": plcsim.HoistWaveAddr, "period_ms": 100, "alarm_windows": 3}}},
	}
	cfgPath := filepath.Join(dir, "gw.json")
	out, _ := json.Marshal(cfg)
	os.WriteFile(cfgPath, out, 0o644)

	pool := x509.NewCertPool()
	ca, _ := os.ReadFile(p.CA)
	pool.AppendCertsFromPEM(ca)
	kp, err := tls.LoadX509KeyPair(p.ClientCert, p.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	preds := map[string]int{}
	var alarms []gateway.Record
	sub, err := mqtt.Connect(mqtt.Options{Addr: b.Addr, ClientID: "e2e-tls-sub", Username: b.User, Password: b.Password,
		TLS: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{kp}},
		OnMessage: func(m mqtt.Message) {
			var recs []gateway.Record
			json.Unmarshal(m.Payload, &recs)
			mu.Lock()
			defer mu.Unlock()
			for _, r := range recs {
				if r.Kind == "prediction" {
					preds[r.Class]++
				}
				if r.Kind == "alarm" && r.Tag == "fault" {
					alarms = append(alarms, r)
				}
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if err := sub.Subscribe("plant1/gw1/data", 1); err != nil {
		t.Fatal(err)
	}
	gw := startGW(t, cfgPath)
	waitFor(t, 15*time.Second, "fault alarm over TLS", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(alarms) > 0 && preds["bearing_inner"] >= 5
	})
	gw.Process.Signal(syscall.SIGTERM)
	gw.Wait()
	mu.Lock()
	defer mu.Unlock()
	if alarms[0].State != "UNACK" || alarms[0].Class != "bearing_inner" {
		t.Fatalf("alarm %+v", alarms[0])
	}
	t.Logf("received over TLS: predictions %v, alarm %s %s", preds, alarms[0].Class, alarms[0].State)
}

// A gateway that requires TLS refuses to start with a plaintext config.
func TestGatewayRefusesPlaintextWhenTLSRequired(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"gateway_id":"gw1","data_dir":"` + dir + `/d","mqtt":{"addr":"127.0.0.1:1883","topic_prefix":"p","require_tls":true},
	  "devices":[{"name":"d1","addr":"127.0.0.1:1","tags":[]}]}`
	path := filepath.Join(dir, "gw.json")
	os.WriteFile(path, []byte(cfg), 0o644)
	cmd := startGW(t, path)
	err := cmd.Wait()
	if err == nil {
		t.Fatal("gateway started without TLS although require_tls is set")
	}
}
