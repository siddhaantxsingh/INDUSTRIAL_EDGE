package analytics

import (
	"math"
	"math/rand"
	"testing"
)

func signal(n int, seed int64) []Point {
	r := rand.New(rand.NewSource(seed))
	pts := make([]Point, n)
	v, slope := 50.0, 0.0
	t := int64(0)
	for i := range pts {
		t += int64(50 + r.Intn(100)) // irregular 50-150 ms sampling
		if r.Float64() < 0.02 {
			slope = r.NormFloat64() * 0.5
		}
		v += slope + r.NormFloat64()*0.2
		if r.Float64() < 0.005 {
			v += r.NormFloat64() * 20 // step change
		}
		pts[i] = Point{T: t, V: v}
	}
	return pts
}

func TestSwingingDoorErrorBoundProperty(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		dev := []float64{0.05, 0.5, 2}[seed%3]
		in := signal(2000, seed)
		sd := SwingingDoor{Dev: dev, MaxGapMs: 60000}
		var out []Point
		for _, p := range in {
			out = append(out, sd.Offer(p)...)
		}
		out = append(out, sd.Flush()...)
		if out[0] != in[0] {
			t.Fatalf("first sample must be archived exactly")
		}
		for _, p := range in {
			if p.T > out[len(out)-1].T {
				break // after the last archived point (nothing to reconstruct)
			}
			if e := math.Abs(Interpolate(out, p.T) - p.V); e > dev*(1+1e-9)+1e-9 {
				t.Fatalf("seed %d dev %g: error %g at t=%d", seed, dev, e, p.T)
			}
		}
		if dev == 2 && len(out) > len(in)/3 {
			t.Fatalf("seed %d: poor compression %d/%d", seed, len(out), len(in))
		}
	}
}

func TestSwingingDoorMaxGapAndFlat(t *testing.T) {
	sd := SwingingDoor{Dev: 1, MaxGapMs: 1000}
	n := 0
	for i := int64(0); i <= 10000; i += 100 {
		n += len(sd.Offer(Point{T: i, V: 5}))
	}
	if n < 10 || n > 12 {
		t.Fatalf("flat signal with 1 s max gap over 10 s archived %d points", n)
	}
}

func TestDeadband(t *testing.T) {
	d := Deadband{Band: 1, MaxGapMs: 1000}
	got := 0
	vals := []float64{0, 0.5, 0.9, 1.1, 1.5, 2.2, 2.2}
	for i, v := range vals {
		got += len(d.Offer(Point{T: int64(i * 100), V: v}))
	}
	if got != 3 { // 0, 1.1, 2.2
		t.Fatalf("deadband emitted %d", got)
	}
}

func TestEWMAFlagsSpikeNotNoise(t *testing.T) {
	e := EWMA{Alpha: 0.05, Warmup: 50}
	r := rand.New(rand.NewSource(1))
	maxNoise := 0.0
	for i := 0; i < 2000; i++ {
		z := math.Abs(e.Score(10 + r.NormFloat64()))
		if i > 100 && z > maxNoise {
			maxNoise = z
		}
	}
	if maxNoise > 5 {
		t.Fatalf("noise produced |z| = %g", maxNoise)
	}
	if z := e.Score(25); z < 8 {
		t.Fatalf("15-sigma spike scored only %g", z)
	}
}

func TestHighAlarmStateMachine(t *testing.T) {
	a := HighAlarm{Limit: 80, Deadband: 5, OnDelayMs: 300, OffDelayMs: 300}
	feed := func(t0 int64, v float64) *Event { return a.Update(Point{T: t0, V: v}) }
	// brief excursion shorter than the on-delay: no alarm
	feed(0, 85)
	feed(200, 85)
	if feed(250, 70); a.State != Normal {
		t.Fatal("chattering excursion raised the alarm")
	}
	feed(1000, 85)
	if ev := feed(1300, 86); ev == nil || ev.To != Unacknowledged {
		t.Fatalf("expected raise after on-delay, got %+v", ev)
	}
	// inside the deadband (78 > 75): stays active
	feed(1400, 78)
	feed(2000, 78)
	if !a.Active() {
		t.Fatal("cleared inside the deadband")
	}
	if ev := a.Ack(2100); ev == nil || a.State != Acknowledged {
		t.Fatal("ack failed")
	}
	feed(2200, 70)
	if ev := feed(2500, 70); ev == nil || ev.To != Normal {
		t.Fatalf("acked alarm should return to NORMAL, got %+v", ev)
	}
	// raise, clear without ack -> RTN_UNACK, then ack -> NORMAL
	feed(3000, 90)
	feed(3300, 90)
	feed(3400, 60)
	feed(3700, 60)
	if a.State != RTNUnack {
		t.Fatalf("state %v, want RTN_UNACK", a.State)
	}
	a.Ack(3800)
	if a.State != Normal {
		t.Fatal("ack of RTN_UNACK should give NORMAL")
	}
}
