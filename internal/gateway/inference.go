package gateway

import (
	"fmt"
	"log"
	"time"

	"industrial-edge/internal/analytics"
	"industrial-edge/internal/drift"
	"industrial-edge/internal/hoistsim"
	"industrial-edge/internal/modbus"
	"industrial-edge/internal/tinyml"
)

// VibrationConfig enables edge fault classification for a device that
// exposes a vibration window over Modbus input registers:
// seq (window counter, 0 = no window yet), rpm, current (0.01 A), then 1024 int16 samples.
type VibrationConfig struct {
	SeqAddr  uint16 `json:"seq_addr"`
	RPMAddr  uint16 `json:"rpm_addr"`
	CurAddr  uint16 `json:"current_addr"`
	WaveAddr uint16 `json:"wave_addr"`
	PeriodMs int    `json:"period_ms"`
	// AlarmWindows: consecutive non-healthy windows before the fault alarm
	// is raised (the on-delay of an ISA-18.2 alarm), and healthy windows
	// before it clears.
	AlarmWindows int          `json:"alarm_windows"`
	Drift        *DriftConfig `json:"drift"`
}

// DriftConfig configures input (PSI) and confidence (Page-Hinkley) drift alerts.
type DriftConfig struct {
	Window   int     `json:"window"`    // inferences per PSI evaluation window (default 300)
	PSI      float64 `json:"psi"`       // alert threshold (default 0.25)
	PSIClear float64 `json:"psi_clear"` // re-arm threshold (default 0.1)
	PHDelta  float64 `json:"ph_delta"`  // Page-Hinkley tolerance (default 0.01)
	PHLambda float64 `json:"ph_lambda"` // Page-Hinkley threshold (default 8)
}

func (v *VibrationConfig) validate(dev string) error {
	if v.PeriodMs <= 0 {
		v.PeriodMs = 1000
	}
	if v.AlarmWindows <= 0 {
		v.AlarmWindows = 3
	}
	if int(v.WaveAddr)+tinyml.N > 65536 {
		return fmt.Errorf("config: device %s: vibration wave_addr too high", dev)
	}
	if d := v.Drift; d != nil {
		if d.Window == 0 {
			d.Window = 300
		}
		if d.PSI == 0 {
			d.PSI = 0.25
		}
		if d.PSIClear == 0 {
			d.PSIClear = 0.1
		}
		if d.PHDelta == 0 {
			d.PHDelta = 0.01
		}
		if d.PHLambda == 0 {
			d.PHLambda = 8
		}
		if d.Window < 50 || d.PSIClear >= d.PSI {
			return fmt.Errorf("config: device %s: drift window >= 50 and psi_clear < psi required", dev)
		}
	}
	return nil
}

type vibState struct {
	cfg     VibrationConfig
	clf     tinyml.Classifier
	fault   *tagState // pseudo-tag "fault": value = predicted class, carries the alarm
	psi     *drift.PSIMonitor
	ph      *drift.PageHinkley
	lastSeq uint16
	haveSeq bool
}

func newVibState(d *device) *vibState {
	vc := *d.cfg.Vibration
	period := int64(vc.PeriodMs)
	v := &vibState{cfg: vc, fault: &tagState{cfg: TagConfig{Name: "fault", Unit: "class"}, good: true,
		alarm: &analytics.HighAlarm{Limit: 0.5, OnDelayMs: int64(vc.AlarmWindows-1) * period,
			OffDelayMs: int64(vc.AlarmWindows-1) * period}}}
	if dc := vc.Drift; dc != nil {
		v.psi = &drift.PSIMonitor{Ref: tinyml.RefHist, Window: dc.Window, Alert: dc.PSI, Clear: dc.PSIClear,
			Names: tinyml.FeatureNames[:]}
		v.ph = &drift.PageHinkley{Delta: dc.PHDelta, Lambda: dc.PHLambda, Warmup: dc.Window}
	}
	d.tags["fault"] = v.fault
	return v
}

// readWindow reads seq, rpm, current and the waveform, then seq again; a
// window that changed during the read (torn) is reported as ok=false.
func readWindow(c *modbus.Client, vc *VibrationConfig, st *Stats) (seq, rpm, cur uint16, x *[tinyml.N]int16, ok bool, err error) {
	rd := func(addr, n uint16) ([]uint16, error) {
		st.ModbusRequests.Add(1)
		return c.ReadInputRegisters(addr, n)
	}
	h, err := rd(vc.SeqAddr, 1)
	if err != nil {
		return
	}
	seq = h[0]
	r, err := rd(vc.RPMAddr, 1)
	if err != nil {
		return
	}
	cr, err := rd(vc.CurAddr, 1)
	if err != nil {
		return
	}
	rpm, cur = r[0], cr[0]
	x = new([tinyml.N]int16)
	for off := 0; off < tinyml.N; off += modbus.MaxReadRegisters {
		n := min(modbus.MaxReadRegisters, tinyml.N-off)
		vals, e := rd(vc.WaveAddr+uint16(off), uint16(n))
		if e != nil {
			return 0, 0, 0, nil, false, e
		}
		for i, v := range vals {
			x[off+i] = int16(v)
		}
	}
	h2, err := rd(vc.SeqAddr, 1)
	if err != nil {
		return
	}
	return seq, rpm, cur, x, h2[0] == seq, nil
}

// vibLoop polls one device's vibration window every period_ms and runs
// the INT8 classifier on each new window.
func (g *Gateway) vibLoop(d *device) {
	defer g.wg.Done()
	vc := d.vib.cfg
	tk := time.NewTicker(time.Duration(vc.PeriodMs) * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-tk.C:
		}
		seq, rpm, cur, x, ok, err := readWindow(d.client, &vc, &g.Stats)
		if err != nil {
			g.Stats.PollErrors.Add(1)
			continue
		}
		if !ok {
			g.Stats.InferenceTorn.Add(1)
			continue
		}
		g.mu.Lock()
		if seq == 0 || (d.vib.haveSeq && seq == d.vib.lastSeq) {
			g.mu.Unlock()
			continue // sensor has not published yet, or no new window
		}
		d.vib.lastSeq, d.vib.haveSeq = seq, true
		g.infer(d, x, rpm, cur)
		g.mu.Unlock()
	}
}

// infer classifies one window and appends prediction, alarm and drift
// records. Caller holds g.mu.
func (g *Gateway) infer(d *device, x *[tinyml.N]int16, rpm, cur uint16) tinyml.Result {
	v := d.vib
	now := g.Now()
	t0 := time.Now()
	res := v.clf.Classify(x, rpm, cur)
	g.Stats.InferenceNanos.Add(uint64(time.Since(t0).Nanoseconds()))
	g.Stats.Inferences.Add(1)
	name := hoistsim.ClassNames[res.Class]
	g.append(Record{Kind: "prediction", Device: d.cfg.Name, Tag: "fault", T: now, V: float64(res.Class),
		Class: name, Conf: res.Confidence})
	f := v.fault
	f.last, f.lastT = float64(res.Class), now
	faulty := 0.0
	if res.Class != hoistsim.Healthy {
		faulty = 1
	}
	if ev := f.alarm.Update(analytics.Point{T: now, V: faulty}); ev != nil {
		g.Stats.Events.Add(1)
		g.append(Record{Kind: "alarm", Device: d.cfg.Name, Tag: "fault", T: now, V: float64(res.Class),
			State: ev.To.String(), Class: name, Msg: "edge fault classifier: " + name})
	}
	if v.psi != nil {
		if msg, started := v.psi.Add(res.Q[:]); msg != "" {
			if started {
				g.Stats.DriftAlerts.Add(1)
			}
			g.append(Record{Kind: "drift", Device: d.cfg.Name, Tag: "fault", T: now, V: v.psi.LastMax, Msg: msg})
		}
		if v.ph.Add(res.Confidence) {
			g.Stats.DriftAlerts.Add(1)
			g.append(Record{Kind: "drift", Device: d.cfg.Name, Tag: "fault", T: now, V: res.Confidence,
				Msg: "model confidence dropped (Page-Hinkley)"})
			v.ph.Reset()
		}
	}
	if g.Stats.Inferences.Load() == 1 {
		log.Printf("edge inference running on %s: first prediction %s (%.2f)", d.cfg.Name, name, res.Confidence)
	}
	return res
}
