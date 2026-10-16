package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeTypesScaleAndWordOrder(t *testing.T) {
	f := EncodeFloat32(-12.5)
	cases := []struct {
		tc   TagConfig
		regs []uint16
		want float64
	}{
		{TagConfig{Type: "uint16", Scale: 1}, []uint16{65535}, 65535},
		{TagConfig{Type: "int16", Scale: 1}, []uint16{65535}, -1},
		{TagConfig{Type: "int16", Scale: 0.1, Offset: 2}, []uint16{100}, 12},
		{TagConfig{Type: "uint32", Scale: 1}, []uint16{1, 2}, 65538},
		{TagConfig{Type: "uint32", Scale: 1, WordSwap: true}, []uint16{2, 1}, 65538},
		{TagConfig{Type: "int32", Scale: 1}, []uint16{0xFFFF, 0xFFFE}, -2},
		{TagConfig{Type: "float32", Scale: 1}, f[:], -12.5},
		{TagConfig{Type: "bool", Scale: 1}, []uint16{1}, 1},
	}
	for i, c := range cases {
		if got := Decode(c.tc, c.regs); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestCoalesceMinimisesRequests(t *testing.T) {
	mk := func(table string, addr uint16, typ string) *tagState {
		n, _ := regs(typ)
		return &tagState{cfg: TagConfig{Table: table, Addr: addr, Type: typ}, n: n}
	}
	ts := []*tagState{
		mk("input", 0, "float32"), mk("input", 2, "float32"), mk("input", 10, "uint16"), // gap 6: bridged
		mk("input", 40, "uint16"),                                                            // gap 29: new block
		mk("holding", 0, "uint16"), mk("holding", 4, "uint16"), mk("holding", 124, "uint16"), // 0+4 merge; 124: gap > 8
		mk("holding", 200, "int32"),
		mk("coil", 0, "bool"), mk("coil", 7, "bool"),
	}
	bs := coalesce(ts)
	// input{0,2,10} input{40} holding{0,4} holding{124} holding{200} coil{0,7}
	if len(bs) != 6 {
		t.Fatalf("got %d blocks: %+v", len(bs), bs)
	}
	for _, b := range bs {
		for _, tg := range b.tags {
			if tg.cfg.Addr < b.start || tg.cfg.Addr+tg.n > b.start+b.count {
				t.Fatalf("tag at %d outside block %d+%d", tg.cfg.Addr, b.start, b.count)
			}
		}
		if b.table != "coil" && b.count > 125 {
			t.Fatalf("block of %d registers", b.count)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	good, _ := os.ReadFile("../../configs/example.json")
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	os.WriteFile(p, good, 0o644)
	if _, err := LoadConfig(p); err != nil {
		t.Fatalf("example config rejected: %v", err)
	}
	bad := map[string]string{
		`"type": "uint32"`:        `"type": "uint64"`,
		`"table": "coil"`:         `"table": "holding"`,
		`"gateway_id": "gw1"`:     `"gateway_id": "gw 1/../x"`,
		`"type": "swinging_door"`: `"type": "magic"`,
		`"poll_ms": 100`:          `"poll_ms": 100, "pol_ms": 5`,
	}
	for from, to := range bad {
		os.WriteFile(p, []byte(strings.Replace(string(good), from, to, 1)), 0o644)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("accepted config with %s", to)
		}
	}
}

func TestConfigRefusesPlaintextWhenTLSRequired(t *testing.T) {
	base := func() *Config {
		return &Config{GatewayID: "gw", DataDir: "d", MQTT: MQTTConfig{Addr: "b:8883", TopicPrefix: "p"},
			Devices: []DeviceConfig{{Name: "d1", Addr: "x:502"}}}
	}
	c := base()
	c.MQTT.RequireTLS = true
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "refusing plaintext") {
		t.Fatalf("require_tls without tls: %v", err)
	}
	c = base()
	c.MQTT.Username = "gw1"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "clear text") {
		t.Fatalf("credentials over plaintext: %v", err)
	}
	c = base()
	c.MQTT.TLS = &TLSConfig{}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "ca_file") {
		t.Fatalf("tls without CA: %v", err)
	}
	c = base()
	c.MQTT.RequireTLS, c.MQTT.Username = true, "gw1"
	c.MQTT.TLS = &TLSConfig{CAFile: "ca.pem", CertFile: "c.pem", KeyFile: "c.key"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(c); err == nil || !strings.Contains(err.Error(), "ca.pem") {
		t.Fatalf("missing CA file must fail at start-up: %v", err)
	}
}
