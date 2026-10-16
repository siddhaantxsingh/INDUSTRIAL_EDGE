package drift

import (
	"math/rand"
	"strings"
	"testing"

	"industrial-edge/internal/hoistsim"
	"industrial-edge/internal/tinyml"
)

func TestPSIBasics(t *testing.T) {
	ref := []float64{0.25, 0.25, 0.25, 0.25}
	if p := PSI(ref, []int{25, 25, 25, 25}, 100); p != 0 {
		t.Fatalf("identical distribution PSI %v", p)
	}
	if p := PSI(ref, []int{70, 10, 10, 10}, 100); p < 0.25 {
		t.Fatalf("large shift PSI %v", p)
	}
}

// stream classifies simulated windows: healthy hoists at training speeds
// unless gain/speed are changed, and feeds the drift detectors.
// rpmLo < 0 means the training speed bands (600-1000 and 1300-1750 rpm).
func stream(t *testing.T, n int, seed int64, gain, rpmLo, rpmHi float64, m *PSIMonitor, ph *PageHinkley) (psiAlerts []string, phAt int) {
	r := rand.New(rand.NewSource(seed))
	var c tinyml.Classifier
	phAt = -1
	for i := 0; i < n; i++ {
		lo, hi := rpmLo, rpmHi
		if lo < 0 {
			lo, hi = 600, 1000
			if r.Intn(2) == 1 {
				lo, hi = 1300, 1750
			}
		}
		cd := hoistsim.RandomCondition(r, r.Intn(hoistsim.NumClasses), lo, hi, 0.1, 1)
		cd.Gain *= gain
		w := hoistsim.Generate(r, cd)
		res := c.Classify(&w.X, w.RPM, w.Current)
		if msg, started := m.Add(res.Q[:]); started {
			psiAlerts = append(psiAlerts, msg)
		}
		if ph.Add(res.Confidence) && phAt < 0 {
			phAt = i
		}
	}
	return
}

func newMon() (*PSIMonitor, *PageHinkley) {
	return &PSIMonitor{Ref: tinyml.RefHist, Window: 300, Alert: 0.25, Clear: 0.1, Names: tinyml.FeatureNames[:]},
		&PageHinkley{Delta: 0.01, Lambda: 8, Warmup: 100}
}

// No drift: data like the training set (same speed bands, same class mix,
// gain spread) must not raise alerts over 1,500 windows.
func TestNoDriftNoAlert(t *testing.T) {
	m, ph := newMon()
	alerts, phAt := stream(t, 1500, 3, 1, -1, 0, m, ph)
	if len(alerts) > 0 || phAt >= 0 {
		t.Fatalf("false alerts: psi %v, page-hinkley at %d (last max PSI %.3f)", alerts, phAt, m.LastMax)
	}
	t.Logf("max PSI at end %.3f", m.LastMax)
}

// Injected drift: after 600 normal windows, the accelerometer gain drops
// to 30 % (e.g. a loosened sensor mount or wrong sensitivity setting).
func TestSensorGainDriftDetected(t *testing.T) {
	m, ph := newMon()
	if a, at := stream(t, 600, 4, 1, -1, 0, m, ph); len(a) > 0 || at >= 0 {
		t.Fatalf("alert before drift: %v %d", a, at)
	}
	alerts, phAt := stream(t, 600, 5, 0.3, -1, 0, m, ph)
	if len(alerts) == 0 {
		t.Fatalf("PSI did not detect a 70 %% gain loss (max PSI %.3f)", m.LastMax)
	}
	if !strings.Contains(alerts[0], "PSI") {
		t.Fatal(alerts[0])
	}
	t.Logf("PSI alert: %s; Page-Hinkley on confidence fired at window %d after the change (-1 = never)", alerts[0], phAt)
}

// Injected drift: speeds move far outside the training range (300-500 rpm).
func TestSpeedRangeDriftDetected(t *testing.T) {
	m, ph := newMon()
	stream(t, 600, 6, 1, -1, 0, m, ph)
	alerts, _ := stream(t, 600, 7, 1, 300, 500, m, ph)
	if len(alerts) == 0 {
		t.Fatalf("PSI did not detect an out-of-range speed band (max PSI %.3f)", m.LastMax)
	}
	t.Log(alerts[0])
}

func TestPageHinkleyDetectsConfidenceDrop(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	ph := &PageHinkley{Delta: 0.01, Lambda: 8, Warmup: 100}
	for i := 0; i < 2000; i++ {
		if ph.Add(0.95 + 0.03*r.NormFloat64()) {
			t.Fatalf("false alarm at %d", i)
		}
	}
	at := -1
	for i := 0; i < 500; i++ {
		if ph.Add(0.75+0.1*r.NormFloat64()) && at < 0 {
			at = i
		}
	}
	if at < 0 || at > 100 {
		t.Fatalf("drop detected at %d", at)
	}
	t.Logf("confidence drop 0.95 -> 0.75 detected after %d samples", at)
}
