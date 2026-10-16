// Package analytics holds the gateway's streaming edge computations:
// lossy-but-bounded compression (deadband, swinging door), an EWMA z-score
// anomaly detector, and an alarm state machine with hysteresis and on/off
// delays modelled on ISA-18.2.
package analytics

import "math"

// Point is one timestamped value (milliseconds since the epoch).
type Point struct {
	T int64
	V float64
}

// Deadband emits a point only when it differs from the last emitted value by
// more than Band (absolute), or when MaxGapMs has elapsed (heartbeat).
type Deadband struct {
	Band     float64
	MaxGapMs int64
	last     *Point
}

// Offer returns the points to emit for one new sample.
func (d *Deadband) Offer(p Point) []Point {
	if d.last == nil || math.Abs(p.V-d.last.V) > d.Band || (d.MaxGapMs > 0 && p.T-d.last.T >= d.MaxGapMs) {
		q := p
		d.last = &q
		return []Point{p}
	}
	return nil
}

// SwingingDoor implements swinging-door trending (Bristol, 1990) in its
// "feasible slope" form: from the last archived point it keeps the interval
// of slopes whose line stays within ±Dev of every sample since; when a new
// sample empties the interval, it archives the point at the previous
// sample's time on the middle feasible slope (within Dev of the true value)
// and restarts from there. Archived points joined by straight lines therefore
// reproduce every input sample to within ±Dev, which property tests check.
// (Archiving the raw previous sample, as often described, can violate the
// bound for samples in between.) MaxGapMs forces an archive point after that
// much time, so a flat signal still reports.
type SwingingDoor struct {
	Dev      float64
	MaxGapMs int64

	anchor  *Point // last archived point
	prev    Point  // last sample seen
	have    bool
	slopeLo float64
	slopeHi float64
}

// Offer processes one sample and returns the points archived as a result.
func (s *SwingingDoor) Offer(p Point) []Point {
	if s.anchor == nil {
		a := p
		s.anchor = &a
		s.have = false
		return []Point{p}
	}
	dt := float64(p.T - s.anchor.T)
	if dt <= 0 {
		return nil // out-of-order or duplicate timestamp: ignore
	}
	lo := (p.V - s.Dev - s.anchor.V) / dt
	hi := (p.V + s.Dev - s.anchor.V) / dt
	if !s.have {
		s.slopeLo, s.slopeHi, s.have = lo, hi, true
		s.prev = p
		if s.MaxGapMs > 0 && p.T-s.anchor.T >= s.MaxGapMs {
			return s.archive(p)
		}
		return nil
	}
	nlo, nhi := math.Max(s.slopeLo, lo), math.Min(s.slopeHi, hi)
	if nlo > nhi || (s.MaxGapMs > 0 && p.T-s.anchor.T >= s.MaxGapMs) {
		// the doors closed: archive the point on a feasible line at the
		// previous sample's time and restart from it
		out := s.archive(s.feasible())
		a := *s.anchor
		dt2 := float64(p.T - a.T)
		s.slopeLo = (p.V - s.Dev - a.V) / dt2
		s.slopeHi = (p.V + s.Dev - a.V) / dt2
		s.have = true
		s.prev = p
		return out
	}
	s.slopeLo, s.slopeHi = nlo, nhi
	s.prev = p
	return nil
}

// feasible is the point at prev.T on the middle of the feasible slope interval.
func (s *SwingingDoor) feasible() Point {
	k := (s.slopeLo + s.slopeHi) / 2
	return Point{T: s.prev.T, V: s.anchor.V + k*float64(s.prev.T-s.anchor.T)}
}

func (s *SwingingDoor) archive(p Point) []Point {
	a := p
	s.anchor = &a
	s.have = false
	return []Point{p}
}

// Flush archives the last pending sample (call at shutdown).
func (s *SwingingDoor) Flush() []Point {
	if s.have {
		return s.archive(s.feasible())
	}
	return nil
}

// Interpolate evaluates the piecewise-linear reconstruction at time t.
func Interpolate(pts []Point, t int64) float64 {
	if len(pts) == 0 {
		return math.NaN()
	}
	if t <= pts[0].T {
		return pts[0].V
	}
	for i := 1; i < len(pts); i++ {
		if t <= pts[i].T {
			a, b := pts[i-1], pts[i]
			return a.V + (b.V-a.V)*float64(t-a.T)/float64(b.T-a.T)
		}
	}
	return pts[len(pts)-1].V
}

// EWMA tracks an exponentially weighted mean and variance and scores each
// sample as a z-score against the statistics *before* the sample, so a
// spike does not hide itself.
type EWMA struct {
	Alpha  float64 // smoothing (e.g. 0.05)
	Warmup int     // samples before scores are reported
	n      int
	mean   float64
	varEst float64
}

// Score returns the z-score of v (0 during warm-up) and updates the statistics.
func (e *EWMA) Score(v float64) float64 {
	e.n++
	if e.n == 1 {
		e.mean = v
		return 0
	}
	sd := math.Sqrt(e.varEst)
	z := 0.0
	if e.n > e.Warmup && sd > 1e-12 {
		z = (v - e.mean) / sd
	}
	d := v - e.mean
	e.mean += e.Alpha * d
	e.varEst = (1 - e.Alpha) * (e.varEst + e.Alpha*d*d)
	return z
}

// AlarmState follows ISA-18.2: Normal, Unacknowledged (active, not acked),
// Acknowledged (active, acked), RTNUnack (returned to normal, not acked).
type AlarmState int

const (
	Normal AlarmState = iota
	Unacknowledged
	Acknowledged
	RTNUnack
)

func (s AlarmState) String() string {
	return [...]string{"NORMAL", "UNACK", "ACKED", "RTN_UNACK"}[s]
}

// HighAlarm raises when the value stays above Limit for OnDelayMs, and
// clears when it stays below Limit-Deadband for OffDelayMs.
type HighAlarm struct {
	Limit, Deadband       float64
	OnDelayMs, OffDelayMs int64

	State      AlarmState
	active     bool
	pendingSet int64 // time the raise condition started (-1 none)
	pendingClr int64
	init       bool
}

// Event is a state change.
type Event struct {
	T        int64
	From, To AlarmState
	Value    float64
}

// Update feeds one sample; returns a transition if one happened.
func (a *HighAlarm) Update(p Point) *Event {
	if !a.init {
		a.pendingSet, a.pendingClr, a.init = -1, -1, true
	}
	prev := a.State
	if !a.active {
		if p.V > a.Limit {
			if a.pendingSet < 0 {
				a.pendingSet = p.T
			}
			if p.T-a.pendingSet >= a.OnDelayMs {
				a.active = true
				a.pendingSet = -1
				if a.State == Normal || a.State == RTNUnack {
					a.State = Unacknowledged
				}
			}
		} else {
			a.pendingSet = -1
		}
	} else {
		if p.V < a.Limit-a.Deadband {
			if a.pendingClr < 0 {
				a.pendingClr = p.T
			}
			if p.T-a.pendingClr >= a.OffDelayMs {
				a.active = false
				a.pendingClr = -1
				switch a.State {
				case Unacknowledged:
					a.State = RTNUnack
				case Acknowledged:
					a.State = Normal
				}
			}
		} else {
			a.pendingClr = -1
		}
	}
	if a.State != prev {
		return &Event{T: p.T, From: prev, To: a.State, Value: p.V}
	}
	return nil
}

// Ack acknowledges the alarm (operator action).
func (a *HighAlarm) Ack(t int64) *Event {
	prev := a.State
	switch a.State {
	case Unacknowledged:
		a.State = Acknowledged
	case RTNUnack:
		a.State = Normal
	default:
		return nil
	}
	return &Event{T: t, From: prev, To: a.State}
}

// Active reports whether the alarm condition is currently active.
func (a *HighAlarm) Active() bool { return a.active }
