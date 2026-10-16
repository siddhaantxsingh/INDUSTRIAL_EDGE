// Package gateway wires the edge pipeline together:
//
//	poller (per device) -> decode tags -> anomaly / alarm / compression
//	    -> durable queue (group-commit fsync) -> uplink (MQTT QoS 1)
//
// Every queued record carries its queue offset as "seq". The uplink commits
// the queue offset only after the broker acknowledged the batch, so delivery
// is at-least-once across broker outages and gateway crashes. Consumers
// de-duplicate on (gateway_id, seq).
package gateway

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"industrial-edge/internal/analytics"
	"industrial-edge/internal/modbus"
	"industrial-edge/internal/mqtt"
	"industrial-edge/internal/queue"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// Record is the JSON form of one queued item.
type Record struct {
	Seq     uint64  `json:"seq"`
	Kind    string  `json:"kind"` // sample | quality | alarm | anomaly | prediction | drift
	Device  string  `json:"dev"`
	Tag     string  `json:"tag,omitempty"`
	T       int64   `json:"t"`
	V       float64 `json:"v"`
	Quality string  `json:"q,omitempty"` // good | bad
	State   string  `json:"state,omitempty"`
	Z       float64 `json:"z,omitempty"`
	Msg     string  `json:"msg,omitempty"`
	Class   string  `json:"class,omitempty"` // predicted fault class (prediction, fault alarm)
	Conf    float64 `json:"conf,omitempty"`  // model confidence (softmax maximum)
}

// Stats are exported as Prometheus metrics.
type Stats struct {
	Polls, PollErrors, Samples, Archived, Events atomic.Uint64
	Published, PublishErrors, Reconnects         atomic.Uint64
	ModbusRequests                               atomic.Uint64
	Inferences, InferenceNanos, InferenceTorn    atomic.Uint64
	DriftAlerts                                  atomic.Uint64
}

type tagState struct {
	cfg   TagConfig
	n     uint16
	sd    *analytics.SwingingDoor
	db    *analytics.Deadband
	alarm *analytics.HighAlarm
	ewma  *analytics.EWMA
	last  float64
	lastT int64
	good  bool
}

type block struct {
	table string
	start uint16
	count uint16
	tags  []*tagState
}

type device struct {
	cfg    DeviceConfig
	client *modbus.Client
	blocks []block
	tags   map[string]*tagState
	up     bool
	lastOK atomic.Int64
	vib    *vibState
}

// Gateway is the running pipeline.
type Gateway struct {
	Cfg   *Config
	Q     *queue.Queue
	Stats Stats
	Now   func() int64 // ms clock (overridable in tests)

	mu      sync.Mutex // serialises queue appends and tag state
	devices []*device
	stop    chan struct{}
	wg      sync.WaitGroup
	uplink  *Uplink
	// mqttOpts: connection options incl. TLS and credentials
	mqttOpts mqtt.Options
}

// New opens the queue and prepares devices (no I/O to devices yet).
func New(cfg *Config) (*Gateway, error) {
	mo, err := mqttOptions(cfg.MQTT)
	if err != nil {
		return nil, err
	}
	q, err := queue.Open(cfg.DataDir+"/queue", queue.Options{SegmentBytes: cfg.Queue.SegmentBytes, MaxBytes: cfg.Queue.MaxBytes})
	if err != nil {
		return nil, err
	}
	g := &Gateway{Cfg: cfg, Q: q, mqttOpts: mo, stop: make(chan struct{}), Now: func() int64 { return time.Now().UnixMilli() }}
	for _, dc := range cfg.Devices {
		d := &device{cfg: dc, client: modbus.NewClient(dc.Addr, dc.Unit, time.Duration(dc.TimeoutMs)*time.Millisecond),
			tags: map[string]*tagState{}}
		var ts []*tagState
		for _, tc := range dc.Tags {
			n, _ := regs(tc.Type)
			s := &tagState{cfg: tc, n: n, good: true}
			if c := tc.Compression; c != nil {
				switch c.Type {
				case "swinging_door":
					s.sd = &analytics.SwingingDoor{Dev: c.Dev, MaxGapMs: c.MaxGapMs}
				case "deadband":
					s.db = &analytics.Deadband{Band: c.Dev, MaxGapMs: c.MaxGapMs}
				}
			}
			if a := tc.Alarm; a != nil {
				s.alarm = &analytics.HighAlarm{Limit: a.High, Deadband: a.Deadband, OnDelayMs: a.OnDelayMs, OffDelayMs: a.OffDelayMs}
			}
			if a := tc.Anomaly; a != nil {
				s.ewma = &analytics.EWMA{Alpha: a.Alpha, Warmup: a.Warmup}
			}
			d.tags[tc.Name] = s
			ts = append(ts, s)
		}
		d.blocks = coalesce(ts)
		if dc.Vibration != nil {
			d.vib = newVibState(d)
		}
		g.devices = append(g.devices, d)
	}
	return g, nil
}

// coalesce groups tags into the fewest block reads: same table, addresses
// sorted, gaps of up to 8 unused registers bridged, at most 125 registers
// (or 2000 coils) per read.
func coalesce(ts []*tagState) []block {
	by := map[string][]*tagState{}
	for _, t := range ts {
		by[t.cfg.Table] = append(by[t.cfg.Table], t)
	}
	var out []block
	for _, table := range []string{"holding", "input", "coil"} {
		list := by[table]
		sort.Slice(list, func(i, j int) bool { return list[i].cfg.Addr < list[j].cfg.Addr })
		limit := uint16(modbus.MaxReadRegisters)
		if table == "coil" {
			limit = modbus.MaxReadCoils
		}
		var cur *block
		for _, t := range list {
			end := t.cfg.Addr + t.n
			if cur != nil && t.cfg.Addr <= cur.start+cur.count+8 && end-cur.start <= limit {
				if end-cur.start > cur.count {
					cur.count = end - cur.start
				}
				cur.tags = append(cur.tags, t)
				continue
			}
			out = append(out, block{table: table, start: t.cfg.Addr, count: t.n, tags: []*tagState{t}})
			cur = &out[len(out)-1]
		}
	}
	return out
}

// Decode converts raw registers to an engineering value.
func Decode(tc TagConfig, r []uint16) float64 {
	var v float64
	w0, w1 := uint32(0), uint32(0)
	if len(r) == 2 {
		w0, w1 = uint32(r[0]), uint32(r[1])
		if tc.WordSwap {
			w0, w1 = w1, w0
		}
	}
	switch tc.Type {
	case "uint16", "bool":
		v = float64(r[0])
	case "int16":
		v = float64(int16(r[0]))
	case "uint32":
		v = float64(w0<<16 | w1)
	case "int32":
		v = float64(int32(w0<<16 | w1))
	case "float32":
		v = float64(math.Float32frombits(w0<<16 | w1))
	}
	return v*tc.Scale + tc.Offset
}

// Encode is the inverse of Decode for float32 (used by the simulator).
func EncodeFloat32(v float32) [2]uint16 {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], math.Float32bits(v))
	return [2]uint16{binary.BigEndian.Uint16(b[:]), binary.BigEndian.Uint16(b[2:])}
}

func (g *Gateway) append(r Record) {
	b, _ := json.Marshal(r)
	if _, err := g.Q.Append(b); err != nil {
		log.Printf("queue append failed: %v", err)
	}
}

// pollOnce reads every block of one device and processes the samples.
func (g *Gateway) pollOnce(d *device) {
	now := g.Now()
	g.Stats.Polls.Add(1)
	type res struct {
		b    block
		vals []uint16
		err  error
	}
	var results []res
	for _, b := range d.blocks {
		var vals []uint16
		var err error
		g.Stats.ModbusRequests.Add(1)
		switch b.table {
		case "holding":
			vals, err = d.client.ReadHoldingRegisters(b.start, b.count)
		case "input":
			vals, err = d.client.ReadInputRegisters(b.start, b.count)
		case "coil":
			vals, err = d.client.ReadCoils(b.start, b.count)
		}
		results = append(results, res{b, vals, err})
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	allOK := true
	for _, r := range results {
		if r.err != nil {
			allOK = false
			g.Stats.PollErrors.Add(1)
			for _, t := range r.b.tags {
				if t.good {
					t.good = false
					g.append(Record{Kind: "quality", Device: d.cfg.Name, Tag: t.cfg.Name, T: now, Quality: "bad", Msg: r.err.Error()})
				}
			}
			continue
		}
		for _, t := range r.b.tags {
			off := t.cfg.Addr - r.b.start
			v := Decode(t.cfg, r.vals[off:off+t.n])
			g.process(d, t, now, v)
		}
	}
	if allOK {
		d.lastOK.Store(now)
	}
}

func (g *Gateway) process(d *device, t *tagState, now int64, v float64) {
	g.Stats.Samples.Add(1)
	if !t.good {
		t.good = true
		g.append(Record{Kind: "quality", Device: d.cfg.Name, Tag: t.cfg.Name, T: now, Quality: "good"})
	}
	t.last, t.lastT = v, now
	p := analytics.Point{T: now, V: v}
	if t.ewma != nil {
		if z := t.ewma.Score(v); math.Abs(z) >= t.cfg.Anomaly.Z {
			g.Stats.Events.Add(1)
			g.append(Record{Kind: "anomaly", Device: d.cfg.Name, Tag: t.cfg.Name, T: now, V: v, Z: z})
		}
	}
	if t.alarm != nil {
		if ev := t.alarm.Update(p); ev != nil {
			g.Stats.Events.Add(1)
			g.append(Record{Kind: "alarm", Device: d.cfg.Name, Tag: t.cfg.Name, T: now, V: v, State: ev.To.String()})
		}
	}
	var pts []analytics.Point
	switch {
	case t.sd != nil:
		pts = t.sd.Offer(p)
	case t.db != nil:
		pts = t.db.Offer(p)
	default:
		pts = []analytics.Point{p}
	}
	for _, q := range pts {
		g.Stats.Archived.Add(1)
		g.append(Record{Kind: "sample", Device: d.cfg.Name, Tag: t.cfg.Name, T: q.T, V: q.V, Quality: "good"})
	}
}

// AckAlarm acknowledges a tag's alarm (operator action from the HTTP API).
func (g *Gateway) AckAlarm(dev, tag string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, d := range g.devices {
		if d.cfg.Name != dev {
			continue
		}
		t := d.tags[tag]
		if t == nil || t.alarm == nil {
			return fmt.Errorf("no alarm on %s.%s", dev, tag)
		}
		if ev := t.alarm.Ack(g.Now()); ev != nil {
			g.append(Record{Kind: "alarm", Device: dev, Tag: tag, T: ev.T, State: ev.To.String(), Msg: "acknowledged"})
		}
		return nil
	}
	return fmt.Errorf("no device %s", dev)
}

// Latest returns the current value of every tag.
func (g *Gateway) Latest() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]any{}
	for _, d := range g.devices {
		for name, t := range d.tags {
			e := map[string]any{"v": t.last, "t": t.lastT, "good": t.good, "unit": t.cfg.Unit}
			if t.alarm != nil {
				e["alarm"] = t.alarm.State.String()
			}
			out[d.cfg.Name+"."+name] = e
		}
	}
	return out
}

// Start launches pollers, the queue syncer and the uplink.
func (g *Gateway) Start() {
	for _, d := range g.devices {
		d := d
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			tk := time.NewTicker(time.Duration(d.cfg.PollMs) * time.Millisecond)
			defer tk.Stop()
			for {
				g.pollOnce(d)
				select {
				case <-g.stop:
					return
				case <-tk.C:
				}
			}
		}()
	}
	for _, d := range g.devices {
		if d.vib != nil {
			g.wg.Add(1)
			go g.vibLoop(d)
		}
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		tk := time.NewTicker(time.Duration(g.Cfg.Queue.SyncMs) * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-g.stop:
				return
			case <-tk.C:
				if err := g.Q.Sync(); err != nil {
					log.Printf("queue sync: %v", err)
				}
			}
		}
	}()
	g.uplink = NewUplink(g)
	g.uplink.Start()
}

// Healthy reports whether every device answered recently.
func (g *Gateway) Healthy() (bool, string) {
	now := g.Now()
	for _, d := range g.devices {
		if now-d.lastOK.Load() > int64(5*d.cfg.PollMs)+int64(d.cfg.TimeoutMs) {
			return false, "device " + d.cfg.Name + " not answering"
		}
	}
	return true, "ok"
}

// Stop shuts down cleanly: stops polling, archives pending compressed
// points, gives the uplink up to `drain` to empty the queue, syncs, closes.
func (g *Gateway) Stop(drain time.Duration) {
	close(g.stop)
	g.wg.Wait()
	g.mu.Lock()
	now := g.Now()
	for _, d := range g.devices {
		for name, t := range d.tags {
			if t.sd != nil {
				for _, p := range t.sd.Flush() {
					g.append(Record{Kind: "sample", Device: d.cfg.Name, Tag: name, T: p.T, V: p.V, Quality: "good"})
				}
			}
		}
		d.client.Close()
	}
	_ = now
	g.mu.Unlock()
	_ = g.Q.Sync()
	if g.uplink != nil {
		g.uplink.StopAfterDrain(drain)
	}
	_ = g.Q.Close()
}
