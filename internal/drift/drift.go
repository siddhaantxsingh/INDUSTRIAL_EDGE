// Package drift detects when the data reaching the edge model no longer
// looks like its training data:
//
//   - PSI (population stability index) of each int8 model input over a
//     sliding window, against the training histogram (10 equal bins);
//   - Page-Hinkley test for a sustained drop in the model's confidence
//     (softmax maximum).
//
// Both raise one alert per episode and re-arm after recovering
// (PSI below Clear; Page-Hinkley after Reset).
package drift

import (
	"fmt"
	"math"
)

// PSI computes the population stability index of observed counts against
// reference proportions. Empty bins are floored at eps.
func PSI(ref []float64, counts []int, total int) float64 {
	const eps = 1e-4
	s := 0.0
	for i, r := range ref {
		p := math.Max(float64(counts[i])/float64(total), eps)
		q := math.Max(r, eps)
		s += (p - q) * math.Log(p/q)
	}
	return s
}

// PSIMonitor tracks per-feature PSI over the last Window samples.
type PSIMonitor struct {
	Ref       [][]float64 // per feature: 10 bin proportions of int8 inputs
	Window    int         // samples per evaluation (e.g. 300)
	Alert     float64     // raise at max PSI >= Alert (0.25 is the usual "major shift")
	Clear     float64     // re-arm below Clear (e.g. 0.1)
	Names     []string
	buf       [][]int8
	pos, n    int
	alerting  bool
	LastMax   float64
	LastFeat  int
	evaluated int
}

func bin(v int8) int { return (int(v) + 128) * 10 / 256 }

// Add records one sample (the model's int8 inputs). It returns a message
// when a drift episode starts or ends, else "".
func (m *PSIMonitor) Add(q []int8) (msg string, started bool) {
	if m.buf == nil {
		m.buf = make([][]int8, m.Window)
	}
	m.buf[m.pos] = append(m.buf[m.pos][:0], q...)
	m.pos = (m.pos + 1) % m.Window
	if m.n < m.Window {
		m.n++
	}
	m.evaluated++
	// evaluate when the window is full, then every Window/10 samples
	if m.n < m.Window || m.evaluated%max(1, m.Window/10) != 0 {
		return "", false
	}
	best, bf := 0.0, 0
	counts := make([]int, 10)
	for f := range m.Ref {
		for i := range counts {
			counts[i] = 0
		}
		for _, s := range m.buf {
			counts[bin(s[f])]++
		}
		if p := PSI(m.Ref[f], counts, m.n); p > best {
			best, bf = p, f
		}
	}
	m.LastMax, m.LastFeat = best, bf
	name := fmt.Sprint(bf)
	if bf < len(m.Names) {
		name = m.Names[bf]
	}
	switch {
	case !m.alerting && best >= m.Alert:
		m.alerting = true
		return fmt.Sprintf("input drift: PSI %.2f on %s (threshold %.2f)", best, name, m.Alert), true
	case m.alerting && best < m.Clear:
		m.alerting = false
		return fmt.Sprintf("input drift cleared: max PSI %.2f", best), false
	}
	return "", false
}

// Alerting reports whether a PSI drift episode is open.
func (m *PSIMonitor) Alerting() bool { return m.alerting }

// PageHinkley detects a sustained decrease of a monitored mean (here the
// model confidence). With x̄ the running mean, it accumulates
// U += x̄ - x - Delta and alerts when U - min(U) > Lambda.
type PageHinkley struct {
	Delta, Lambda float64
	Warmup        int // samples before testing
	n             int
	mean, u, umin float64
	alerting      bool
}

// Add feeds one value; returns true when an alert starts.
func (p *PageHinkley) Add(x float64) bool {
	p.n++
	p.mean += (x - p.mean) / float64(p.n)
	if p.n <= p.Warmup {
		return false
	}
	p.u += p.mean - x - p.Delta
	p.umin = math.Min(p.umin, p.u)
	if !p.alerting && p.u-p.umin > p.Lambda {
		p.alerting = true
		return true
	}
	return false
}

// Reset re-arms the detector and restarts its statistics.
func (p *PageHinkley) Reset() { *p = PageHinkley{Delta: p.Delta, Lambda: p.Lambda, Warmup: p.Warmup} }

// Alerting reports whether the detector has fired since the last Reset.
func (p *PageHinkley) Alerting() bool { return p.alerting }
