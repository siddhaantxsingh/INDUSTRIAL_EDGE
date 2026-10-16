// Package hoistsim is a physics-inspired simulator of a crane hoist motor
// with an accelerometer on the drive-end bearing housing and a motor current
// reading from the drive. It produces SYNTHETIC data only: it is used to
// train and test the TinyML fault classifier, to evaluate predictive
// maintenance on simulated run-to-failure degradations, and to feed the PLC
// simulator. It is not real machine data, and results on it say nothing
// definite about real machines.
//
// Model (per 0.2 s window of 1024 samples at 5120 Hz):
//
//   - shaft rate f1 = rpm/60. Healthy machines have a small residual 1x
//     (imbalance, grows with speed squared), a small 2x, a 100 Hz electrical
//     component, and broadband noise that grows with load.
//   - imbalance: extra 1x amplitude, proportional to severity * (f1/25 Hz)^2
//     (centrifugal force grows with the square of speed);
//   - misalignment: extra 2x (dominant), 1x and 3x;
//   - bearing outer-race defect: impacts at BPFO = 3.58 * f1 (with ±1 %
//     slip jitter); each impact rings a structural resonance (1.8–2.6 kHz,
//     decay 1–2 ms);
//   - bearing inner-race defect: impacts at BPFI = 5.42 * f1, amplitude-
//     modulated once per revolution as the defect passes the load zone;
//   - overload: load 1.05–1.5 × rated; current rises in proportion and
//     broadband vibration and 1x/2x rise slightly.
//
// Operating conditions vary per window: speed, load, sensor gain (±20 %),
// noise level and resonance frequency, and all phases are random.
package hoistsim

import (
	"math"
	"math/rand"
)

// Constants shared with the feature extractor and the training scripts.
const (
	N        = 1024   // samples per window
	FS       = 5120.0 // sample rate, Hz
	LSBPerG  = 2000.0 // accelerometer scale: 0.5 mg per LSB
	BPFO     = 3.58   // outer-race defect frequency, in orders of shaft speed
	BPFI     = 5.42   // inner-race defect frequency, in orders of shaft speed
	RatedAmp = 30.0   // rated motor current, A
	// MeshTeeth: gear-mesh frequency of the first hoist gear stage, in orders.
	MeshTeeth = 17
)

// Class labels.
const (
	Healthy = iota
	Imbalance
	Misalignment
	OuterRace
	InnerRace
	Overload
	NumClasses
)

// ClassNames indexes Class labels.
var ClassNames = [NumClasses]string{"healthy", "imbalance", "misalignment", "bearing_outer", "bearing_inner", "overload"}

// Condition is the operating point and fault state for one window.
type Condition struct {
	RPM      float64 // motor speed
	Load     float64 // fraction of rated load
	Class    int     // fault class
	Severity float64 // 0 = none, 1 = severe (classes other than Healthy)
	Gain     float64 // sensor gain multiplier (calibration spread)
	Noise    float64 // broadband noise level, g RMS
	ResHz    float64 // structural resonance excited by bearing impacts
	// Machine-specific baseline (fixed for one machine): multipliers of
	// the residual 1x/2x/3x levels, gear-mesh amplitude (g) and the mean
	// number of random transient knocks (hook, rope, load handling) per window.
	Base1, Base2, Base3 float64
	MeshAmp             float64
	Knocks              float64
}

// Window is one simulated acquisition.
type Window struct {
	X       [N]int16 // acceleration, 0.5 mg per LSB
	RPM     uint16   // speed from the drive
	Current uint16   // RMS motor current, 0.01 A units
	Cond    Condition
}

// RandomCondition draws an operating point for class c with speed in
// [rpmLo, rpmHi) and severity in [sevLo, sevHi).
func RandomCondition(r *rand.Rand, c int, rpmLo, rpmHi, sevLo, sevHi float64) Condition {
	cd := Condition{
		RPM:   rpmLo + r.Float64()*(rpmHi-rpmLo),
		Load:  0.1 + 0.9*r.Float64(),
		Class: c,
	}
	cd.SetMachine(r)
	cd.Noise = 0.02 + 0.04*r.Float64()
	if c != Healthy {
		cd.Severity = sevLo + r.Float64()*(sevHi-sevLo)
	}
	if c == Overload {
		cd.Load = 1.05 + 0.45*r.Float64()
	}
	return cd
}

// SetMachine draws the machine-specific fields (gain, resonance, baseline).
func (cd *Condition) SetMachine(r *rand.Rand) {
	cd.Gain = 0.7 + 0.6*r.Float64()
	cd.ResHz = 1800 + 800*r.Float64()
	cd.Base1 = 0.5 + 1.5*r.Float64()
	cd.Base2 = 0.5 + 1.5*r.Float64()
	cd.Base3 = 0.5 + 1.5*r.Float64()
	cd.MeshAmp = 0.02 + 0.06*r.Float64()
	cd.Knocks = 0.6 * r.Float64()
}

// Generate simulates one window.
func Generate(r *rand.Rand, cd Condition) Window {
	var w Window
	w.Cond = cd
	f1 := cd.RPM / 60
	sp := f1 / 25
	dt := 1 / FS
	load := cd.Load
	// periodic components: amplitudes in g
	a1 := 0.03 * sp * sp * cd.Base1
	a2 := 0.012 * cd.Base2
	a3 := 0.004 * cd.Base3
	if load > 1 {
		a1 += 0.04 * (load - 1)
		a2 += 0.03 * (load - 1)
	}
	switch cd.Class {
	case Imbalance:
		a1 += 0.35 * cd.Severity * sp * sp
	case Misalignment:
		a2 += 0.25 * cd.Severity
		a1 += 0.06 * cd.Severity
		a3 += 0.10 * cd.Severity
	}
	p1, p2, p3, pe := r.Float64()*2*math.Pi, r.Float64()*2*math.Pi, r.Float64()*2*math.Pi, r.Float64()*2*math.Pi
	ae := 0.008 + 0.006*load
	noise := cd.Noise * (0.7 + 0.5*load)
	am := cd.MeshAmp * (0.5 + 0.5*math.Min(load, 1.5))
	pm := r.Float64() * 2 * math.Pi
	sig := make([]float64, N)
	for n := 0; n < N; n++ {
		t := float64(n) * dt
		s := a1*math.Sin(2*math.Pi*f1*t+p1) + a2*math.Sin(2*math.Pi*2*f1*t+p2) + a3*math.Sin(2*math.Pi*3*f1*t+p3) +
			ae*math.Sin(2*math.Pi*100*t+pe) + am*math.Sin(2*math.Pi*MeshTeeth*f1*t+pm)
		sig[n] = s + noise*r.NormFloat64()
	}
	// random transient knocks (load handling): not periodic
	for k := poisson(r, cd.Knocks); k > 0; k-- {
		ring(sig, r.Float64()*float64(N)*dt, 0.2+1.0*r.Float64(), 0.001+0.001*r.Float64(), cd.ResHz*(0.9+0.2*r.Float64()))
	}
	// bearing impacts
	if cd.Class == OuterRace || cd.Class == InnerRace {
		rate := BPFO * f1
		amp := 1.2 * cd.Severity
		if cd.Class == InnerRace {
			rate = BPFI * f1
			amp = 1.0 * cd.Severity
		}
		tau := 0.001 + 0.001*r.Float64()
		period := 1 / rate
		t0 := r.Float64() * period
		pm := r.Float64() * 2 * math.Pi
		for ti := t0; ti < float64(N)*dt; ti += period * (1 + 0.01*r.NormFloat64()) {
			a := amp * (0.8 + 0.4*r.Float64())
			if cd.Class == InnerRace {
				a *= 0.5 * (1 + math.Cos(2*math.Pi*f1*ti+pm))
			}
			ring(sig, ti, a, tau, cd.ResHz)
		}
	}
	for n := 0; n < N; n++ {
		v := math.Round(sig[n] * cd.Gain * LSBPerG)
		w.X[n] = int16(math.Max(-32768, math.Min(32767, v)))
	}
	w.RPM = uint16(math.Round(cd.RPM))
	// current: magnetising + torque-producing part; 2 % measurement noise
	amps := (8 + 22*load) * (0.9 + 0.1*sp) * (1 + 0.03*r.NormFloat64())
	w.Current = uint16(math.Max(0, math.Round(amps*100)))
	return w
}

// ring adds a decaying resonance excited by an impact at time ti.
func ring(sig []float64, ti, a, tau, hz float64) {
	dt := 1 / FS
	for n := int(math.Ceil(ti / dt)); n < len(sig); n++ {
		tt := float64(n)*dt - ti
		if tt > 8*tau {
			break
		}
		sig[n] += a * math.Exp(-tt/tau) * math.Sin(2*math.Pi*hz*tt)
	}
}

func poisson(r *rand.Rand, lambda float64) int {
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

// Dataset generates the training and test sets written by cmd/vibgen.
// Training windows use speeds in [600, 1000) and [1300, 1750) rpm; test
// windows use the held-out band [1000, 1300) rpm. Severity is in [0.1, 1).
// The output depends only on the arguments (deterministic for a seed).
func Dataset(seed int64, nTrain, nTest int) (train, test []Window) {
	r := rand.New(rand.NewSource(seed))
	for c := 0; c < NumClasses; c++ {
		for i := 0; i < nTrain; i++ {
			lo, hi := 600.0, 1000.0
			if r.Intn(2) == 1 {
				lo, hi = 1300, 1750
			}
			train = append(train, Generate(r, RandomCondition(r, c, lo, hi, 0.1, 1.0)))
		}
		for i := 0; i < nTest; i++ {
			test = append(test, Generate(r, RandomCondition(r, c, 1000, 1300, 0.1, 1.0)))
		}
	}
	r.Shuffle(len(train), func(i, j int) { train[i], train[j] = train[j], train[i] })
	return train, test
}
