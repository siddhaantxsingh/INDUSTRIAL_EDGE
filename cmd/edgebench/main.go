// edgebench: reproducible measurements for docs/benchmarks.md.
//
//		edgebench -out bench/results/DATE [-broker 127.0.0.1:1883] [-seconds 20]
//
//	 1. compression: one simulated hour of the tank process sampled at 10 Hz;
//	    swinging door and deadband at several deviations; reduction ratio and
//	    the measured maximum reconstruction error.
//	 2. queue: append throughput of 150-byte records with fsync per record,
//	    group commit every 100 ms, and no fsync; recovery (open) time.
//	 3. pipeline (needs a broker): 20 simulated PLCs x 50 float32 tags polled
//	    every 100 ms (10,000 samples/s) through the full gateway into MQTT; a
//	    subscriber measures delivered records/s and end-to-end latency
//	    (receive time - sample time), and the gateway's CPU use is read from
//	    /proc/self/stat.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"industrial-edge/internal/analytics"
	"industrial-edge/internal/gateway"
	"industrial-edge/internal/modbus"
	"industrial-edge/internal/mqtt"
	"industrial-edge/internal/plcsim"
	"industrial-edge/internal/queue"
)

func main() {
	out := flag.String("out", "bench/results", "output directory")
	broker := flag.String("broker", "", "MQTT broker for the pipeline benchmark (skipped if empty)")
	secs := flag.Int("seconds", 20, "pipeline benchmark duration")
	flag.Parse()
	os.MkdirAll(*out, 0o755)
	write := func(name string, v any) {
		b, _ := json.MarshalIndent(v, "", " ")
		os.WriteFile(filepath.Join(*out, name), b, 0o644)
		fmt.Printf("%s\n%s\n", name, b)
	}
	write("compression.json", compression())
	write("queue.json", queueBench())
	if *broker != "" {
		write("pipeline.json", pipeline(*broker, *secs))
	}
}

// --------------------------------------------------------------- compression

func compression() any {
	p := plcsim.New(7, 0)
	m := p.Server.Model
	series := map[string][]analytics.Point{}
	read := func(i int) float64 {
		return gateway.Decode(gateway.TagConfig{Type: "float32", Scale: 1}, m.Input[2*i:2*i+2])
	}
	for k := 0; k < 36000; k++ { // 1 h at 10 Hz
		if k%3000 == 0 { // the operator moves the valve every 5 minutes
			m.Holding[0] = uint16(20 + (k/3000*37)%70)
		}
		p.Step(0.1)
		t := int64(k) * 100
		for i, name := range []string{"level", "temperature", "outflow"} {
			series[name] = append(series[name], analytics.Point{T: t, V: read(i)})
		}
	}
	type row struct {
		Tag, Method        string
		Dev, Ratio, MaxErr float64
		Kept, Total        int
	}
	var rows []row
	for _, name := range []string{"level", "temperature", "outflow"} {
		in := series[name]
		for _, dev := range []float64{0.01, 0.05, 0.2, 0.5} {
			sd := analytics.SwingingDoor{Dev: dev, MaxGapMs: 60000}
			var outp []analytics.Point
			for _, pt := range in {
				outp = append(outp, sd.Offer(pt)...)
			}
			outp = append(outp, sd.Flush()...)
			maxErr := 0.0
			for _, pt := range in {
				if pt.T > outp[len(outp)-1].T {
					break
				}
				maxErr = math.Max(maxErr, math.Abs(analytics.Interpolate(outp, pt.T)-pt.V))
			}
			rows = append(rows, row{name, "swinging_door", dev, float64(len(in)) / float64(len(outp)), maxErr, len(outp), len(in)})

			db := analytics.Deadband{Band: dev, MaxGapMs: 60000}
			var kept []analytics.Point
			for _, pt := range in {
				kept = append(kept, db.Offer(pt)...)
			}
			// deadband reconstruction is "hold the last value"
			maxErr, j := 0.0, 0
			for _, pt := range in {
				for j+1 < len(kept) && kept[j+1].T <= pt.T {
					j++
				}
				maxErr = math.Max(maxErr, math.Abs(kept[j].V-pt.V))
			}
			rows = append(rows, row{name, "deadband", dev, float64(len(in)) / float64(len(kept)), maxErr, len(kept), len(in)})
		}
	}
	return rows
}

// --------------------------------------------------------------------- queue

func queueBench() any {
	rec := []byte(`{"kind":"sample","dev":"plc07","tag":"tag_042","t":1760000000000,"v":42.1234567,"q":"good","pad":"xxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`)
	type row struct {
		Mode          string
		Records       int
		RecordsPerSec float64
		MBPerSec      float64
	}
	var rows []row
	for _, mode := range []struct {
		name string
		n    int
		opt  queue.Options
		grp  time.Duration
	}{
		{"fsync every record", 2000, queue.Options{SyncEvery: 1}, 0},
		{"group commit (fsync every 100 ms)", 300000, queue.Options{}, 100 * time.Millisecond},
		{"no fsync (OS page cache)", 300000, queue.Options{}, 0},
	} {
		dir, _ := os.MkdirTemp("", "qbench")
		q, _ := queue.Open(dir, mode.opt)
		t0 := time.Now()
		last := t0
		for i := 0; i < mode.n; i++ {
			q.Append(rec)
			if mode.grp > 0 && time.Since(last) >= mode.grp {
				q.Sync()
				last = time.Now()
			}
		}
		q.Sync()
		el := time.Since(t0).Seconds()
		q.Close()
		rows = append(rows, row{mode.name, mode.n, float64(mode.n) / el, float64(mode.n*(len(rec)+8)) / el / 1e6})
		os.RemoveAll(dir)
	}
	// recovery: open a queue holding 1 M records
	dir, _ := os.MkdirTemp("", "qbench")
	defer os.RemoveAll(dir)
	q, _ := queue.Open(dir, queue.Options{})
	for i := 0; i < 1000000; i++ {
		q.Append(rec)
	}
	q.Close()
	t0 := time.Now()
	q2, err := queue.Open(dir, queue.Options{})
	if err != nil {
		log.Fatal(err)
	}
	openS := time.Since(t0).Seconds()
	n := q2.Backlog()
	q2.Close()
	return map[string]any{"append": rows, "recovery": map[string]any{"records": n, "open_seconds": openS}}
}

// ------------------------------------------------------------------ pipeline

func cpuSeconds() float64 {
	b, _ := os.ReadFile("/proc/self/stat")
	f := strings.Fields(string(b)[strings.LastIndex(string(b), ")")+2:])
	ut, _ := strconv.ParseFloat(f[11], 64)
	st, _ := strconv.ParseFloat(f[12], 64)
	return (ut + st) / 100
}

func pipeline(broker string, secs int) any {
	const plcs, tags = 20, 50
	var devs []gateway.DeviceConfig
	var plants []*plcsim.Plant
	for i := 0; i < plcs; i++ {
		p := plcsim.New(int64(i), 0)
		// a larger register map: 50 float32 tags fed with a ramp + noise
		p.Server.Model = modbus.NewDataModel(2 * tags)
		addr, err := p.Server.Listen("127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
		plants = append(plants, p)
		var tc []gateway.TagConfig
		for k := 0; k < tags; k++ {
			tc = append(tc, gateway.TagConfig{Name: fmt.Sprintf("tag_%02d", k), Table: "input", Addr: uint16(2 * k), Type: "float32"})
		}
		devs = append(devs, gateway.DeviceConfig{Name: fmt.Sprintf("plc%02d", i), Addr: addr, PollMs: 100, TimeoutMs: 1000, Tags: tc})
	}
	// keep every value changing so nothing is compressed away
	stop := make(chan struct{})
	go func() {
		k := 0
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			k++
			for _, p := range plants {
				p.Server.Model.Update(func(m *modbus.DataModel) {
					for j := 0; j < tags; j++ {
						w := gateway.EncodeFloat32(float32(k + j))
						m.Input[2*j], m.Input[2*j+1] = w[0], w[1]
					}
				})
			}
		}
	}()
	dir, _ := os.MkdirTemp("", "pipe")
	defer os.RemoveAll(dir)
	cfg := &gateway.Config{GatewayID: "bench", DataDir: dir, MQTT: gateway.MQTTConfig{Addr: broker, TopicPrefix: "bench/gw", BatchMax: 500},
		Queue: gateway.QueueConfig{SyncMs: 100}, Devices: devs}
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	var mu sync.Mutex
	var lat []float64
	received := 0
	sub, err := mqtt.Connect(mqtt.Options{Addr: broker, ClientID: "bench-sub", CleanSession: true, KeepAlive: 30 * time.Second,
		OnMessage: func(m mqtt.Message) {
			now := time.Now().UnixMilli()
			var recs []gateway.Record
			json.Unmarshal(m.Payload, &recs)
			mu.Lock()
			for _, r := range recs {
				lat = append(lat, float64(now-r.T))
				received++
			}
			mu.Unlock()
		}})
	if err != nil {
		log.Fatal(err)
	}
	sub.Subscribe("bench/gw/data", 1)
	g, err := gateway.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	cpu0 := cpuSeconds()
	t0 := time.Now()
	g.Start()
	time.Sleep(time.Duration(secs) * time.Second)
	el := time.Since(t0).Seconds()
	cpu := cpuSeconds() - cpu0
	mu.Lock()
	n := received
	l := append([]float64(nil), lat...)
	mu.Unlock()
	g.Stop(10 * time.Second)
	close(stop)
	for _, p := range plants {
		p.Server.Close()
	}
	sub.Close()
	sort.Float64s(l)
	pct := func(q float64) float64 { return l[int(q*float64(len(l)-1))] }
	return map[string]any{
		"plcs": plcs, "tags": plcs * tags, "poll_ms": 100, "seconds": el,
		"samples_acquired": g.Stats.Samples.Load(), "records_delivered": n,
		"delivered_per_sec": float64(n) / el, "modbus_requests": g.Stats.ModbusRequests.Load(),
		"latency_ms_p50": pct(0.5), "latency_ms_p95": pct(0.95), "latency_ms_p99": pct(0.99), "latency_ms_max": l[len(l)-1],
		"process_cpu_seconds": cpu, "cpu_cores_used": cpu / el,
		"note": "process CPU includes the 20 simulated PLC servers, the value updater and the subscriber",
	}
}
