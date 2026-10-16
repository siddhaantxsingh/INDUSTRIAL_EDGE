package gateway

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"industrial-edge/internal/hoistsim"
	"industrial-edge/internal/plcsim"
)

func runHoist(t *testing.T, class int, sev, gain float64, d time.Duration, drift *DriftConfig) []Record {
	plcsim.HoistPeriod = 40 * time.Millisecond
	p := plcsim.New(1, 0)
	p.SetHoist(class, sev, gain)
	addr, err := p.Listen("127.0.0.1:0", 50)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cfg := &Config{GatewayID: "gw1", DataDir: filepath.Join(t.TempDir(), "d"), MQTT: MQTTConfig{Addr: "127.0.0.1:1", TopicPrefix: "p"},
		Devices: []DeviceConfig{{Name: "hoist1", Addr: addr, PollMs: 100,
			Vibration: &VibrationConfig{SeqAddr: plcsim.HoistSeqAddr, RPMAddr: plcsim.HoistRPMAddr, CurAddr: plcsim.HoistCurAddr,
				WaveAddr: plcsim.HoistWaveAddr, PeriodMs: 20, AlarmWindows: 3, Drift: drift}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	g.Start()
	time.Sleep(d)
	g.Stop(0)
	g2, err := New(cfg) // reopen the queue to read what was recorded
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Q.Close()
	recs, err := g2.Q.Read(g2.Q.AckedOffset(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var out []Record
	for _, r := range recs {
		var rec Record
		if err := json.Unmarshal(r.Payload, &rec); err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

func count(recs []Record) (preds map[string]int, alarms []Record, drifts []Record) {
	preds = map[string]int{}
	for _, r := range recs {
		switch r.Kind {
		case "prediction":
			preds[r.Class]++
		case "alarm":
			if r.Tag == "fault" {
				alarms = append(alarms, r)
			}
		case "drift":
			drifts = append(drifts, r)
		}
	}
	return
}

func TestEdgeInferenceRaisesFaultAlarm(t *testing.T) {
	recs := runHoist(t, hoistsim.OuterRace, 0.8, 1, 1500*time.Millisecond, nil)
	preds, alarms, _ := count(recs)
	n := 0
	for _, v := range preds {
		n += v
	}
	if n < 10 || preds["bearing_outer"] < n*8/10 {
		t.Fatalf("predictions %v", preds)
	}
	if len(alarms) == 0 || alarms[0].State != "UNACK" || alarms[0].Class != "bearing_outer" {
		t.Fatalf("expected an UNACK bearing_outer fault alarm, got %+v", alarms)
	}
	t.Logf("predictions %v; first alarm %+v", preds, alarms[0])
}

func TestEdgeInferenceHealthyNoAlarm(t *testing.T) {
	recs := runHoist(t, hoistsim.Healthy, 0, 1, 1500*time.Millisecond, nil)
	preds, alarms, _ := count(recs)
	if len(alarms) != 0 {
		t.Fatalf("healthy hoist raised %+v (predictions %v)", alarms, preds)
	}
	t.Logf("predictions %v", preds)
}

func TestEdgeInferenceDriftAlert(t *testing.T) {
	// sensor gain at 25 %: inputs leave the training distribution
	recs := runHoist(t, hoistsim.Healthy, 0, 0.25, 4*time.Second, &DriftConfig{Window: 60})
	_, _, drifts := count(recs)
	if len(drifts) == 0 {
		t.Fatal("no drift record")
	}
	t.Logf("drift: %s", drifts[0].Msg)
}
