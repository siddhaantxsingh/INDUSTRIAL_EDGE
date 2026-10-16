// pdmeval evaluates predictive maintenance on SIMULATED run-to-failure
// degradations (internal/hoistsim), comparing the edge INT8 classifier with
// the gateway's existing EWMA z-score anomaly detector.
//
//	go run ./cmd/pdmeval -out bench/results/2026-10-14/pdm.json
//
// Set-up (all synthetic):
//   - one vibration window every -every minutes (default 10);
//   - healthy fleet: -healthy machines for -days days each (false alarms);
//   - run-to-failure: -rtf machines per fault type (imbalance,
//     misalignment, outer race, inner race). Each starts healthy for 2-10
//     days, then severity grows exponentially from 0 to 1.0 over 10-30
//     days; "failure" is the time severity reaches 1.0;
//   - every window draws speed 600-1750 rpm (this includes the 1000-1300 rpm
//     band held out from training), load and noise; machine gain, resonance
//     and baseline levels are fixed per machine.
//
// Window-level ground truth: faulty when severity >= 0.1 (the lowest
// severity in the training set). Windows with 0 < severity < 0.1 are
// incipient and excluded from precision/recall (counted separately).
//
// Detectors and alarm rule (same for both): an alarm is raised after K=3
// consecutive positive windows and clears after 3 consecutive negative ones.
//   - model: positive = predicted class is not healthy;
//   - EWMA: positive = |z| >= threshold of the window's vibration RMS
//     (analytics.EWMA, alpha 0.05, warm-up 50), as the gateway would run it
//     on an RMS tag; thresholds 3 and 6 (the example config uses 6).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"industrial-edge/internal/analytics"
	"industrial-edge/internal/hoistsim"
	"industrial-edge/internal/tinyml"
)

const k = 3 // consecutive windows to raise / clear an alarm

type alarmFSM struct {
	on       bool
	pos, neg int
}

// step returns true when an alarm is raised.
func (a *alarmFSM) step(positive bool) bool {
	if positive {
		a.pos, a.neg = a.pos+1, 0
	} else {
		a.neg, a.pos = a.neg+1, 0
	}
	if !a.on && a.pos >= k {
		a.on = true
		return true
	}
	if a.on && a.neg >= k {
		a.on = false
	}
	return false
}

type detector struct {
	name   string
	ewmaZ  float64 // 0 = model
	ewma   *analytics.EWMA
	alarm  alarmFSM
	tp, fp int
	fn, tn int
}

func (d *detector) reset() {
	d.alarm = alarmFSM{}
	if d.ewmaZ > 0 {
		d.ewma = &analytics.EWMA{Alpha: 0.05, Warmup: 50}
	}
}

type window struct {
	pred int
	rms  float64
}

func rmsG(x *[hoistsim.N]int16) float64 {
	var m, s float64
	for _, v := range x {
		m += float64(v)
	}
	m /= hoistsim.N
	for _, v := range x {
		d := float64(v) - m
		s += d * d
	}
	return math.Sqrt(s/hoistsim.N) / hoistsim.LSBPerG
}

func (d *detector) positive(w window) bool {
	if d.ewmaZ == 0 {
		return w.pred != hoistsim.Healthy
	}
	return math.Abs(d.ewma.Score(w.rms)) >= d.ewmaZ
}

type rtfResult struct {
	Fault      string             `json:"fault"`
	Machine    int                `json:"machine"`
	StartDay   float64            `json:"start_day"` // degradation starts
	FailureDay float64            `json:"failure_day"`
	OnsetDay   float64            `json:"onset_day"`  // severity reaches 0.1
	DetectDay  map[string]float64 `json:"detect_day"` // first alarm at or after degradation start; NaN = missed
	DetectSev  map[string]float64 `json:"detect_severity"`
}

func main() {
	out := flag.String("out", "", "write JSON results here")
	every := flag.Float64("every", 10, "minutes between vibration windows")
	healthy := flag.Int("healthy", 30, "healthy machines")
	days := flag.Float64("days", 30, "days per healthy machine")
	rtf := flag.Int("rtf", 25, "run-to-failure machines per fault type")
	seed := flag.Int64("seed", 11, "random seed (different from the training seed)")
	flag.Parse()
	r := rand.New(rand.NewSource(*seed))
	perDay := 24 * 60 / *every
	var clf tinyml.Classifier
	dets := []*detector{{name: "model"}, {name: "ewma_z3", ewmaZ: 3}, {name: "ewma_z6", ewmaZ: 6}}
	gen := func(cd hoistsim.Condition) window {
		lo := 600 + r.Float64()*1150
		cd.RPM = lo
		cd.Load = 0.1 + 0.9*r.Float64()
		cd.Noise = 0.02 + 0.04*r.Float64()
		w := hoistsim.Generate(r, cd)
		res := clf.Classify(&w.X, w.RPM, w.Current)
		return window{pred: res.Class, rms: rmsG(&w.X)}
	}
	// confusion matrix of the model over all scored windows
	var conf [hoistsim.NumClasses][hoistsim.NumClasses]int
	falseAlarms := map[string]int{}
	// ---- healthy fleet ----
	nH := int(*days * perDay)
	for m := 0; m < *healthy; m++ {
		var cd hoistsim.Condition
		cd.SetMachine(r)
		for _, d := range dets {
			d.reset()
		}
		for i := 0; i < nH; i++ {
			w := gen(cd)
			conf[hoistsim.Healthy][w.pred]++
			for _, d := range dets {
				p := d.positive(w)
				if d.alarm.step(p) {
					falseAlarms[d.name]++
				}
				if p {
					d.fp++
				} else {
					d.tn++
				}
			}
		}
	}
	// ---- run-to-failure ----
	var results []rtfResult
	incipient := 0
	faults := []int{hoistsim.Imbalance, hoistsim.Misalignment, hoistsim.OuterRace, hoistsim.InnerRace}
	for _, f := range faults {
		for m := 0; m < *rtf; m++ {
			var cd hoistsim.Condition
			cd.SetMachine(r)
			start := 2 + 8*r.Float64()
			dur := 10 + 20*r.Float64()
			growth := 3 + 3*r.Float64() // exponential growth rate over the degradation
			res := rtfResult{Fault: hoistsim.ClassNames[f], Machine: m, StartDay: start, FailureDay: start + dur,
				DetectDay: map[string]float64{}, DetectSev: map[string]float64{}}
			// severity(t) = (exp(g u) - 1) / (exp(g) - 1), u = (t - start)/dur
			res.OnsetDay = start + dur*math.Log(1+0.1*(math.Exp(growth)-1))/growth
			for _, d := range dets {
				d.reset()
				res.DetectDay[d.name] = math.NaN()
				res.DetectSev[d.name] = math.NaN()
			}
			n := int((start + dur) * perDay)
			for i := 0; i < n; i++ {
				t := float64(i) / perDay
				sev := 0.0
				if t >= start {
					u := (t - start) / dur
					sev = (math.Exp(growth*u) - 1) / (math.Exp(growth) - 1)
				}
				c := cd
				if sev > 0 {
					c.Class, c.Severity = f, sev
				}
				w := gen(c)
				truth := sev >= 0.1
				if sev > 0 && !truth {
					incipient++
				}
				if truth {
					conf[f][w.pred]++
				} else if sev == 0 {
					conf[hoistsim.Healthy][w.pred]++
				}
				for _, d := range dets {
					p := d.positive(w)
					raised := d.alarm.step(p)
					if raised {
						if t < start {
							falseAlarms[d.name]++
						} else if math.IsNaN(res.DetectDay[d.name]) {
							res.DetectDay[d.name] = t
							res.DetectSev[d.name] = sev
						}
					}
					switch {
					case sev > 0 && !truth: // incipient: not scored
					case truth && p:
						d.tp++
					case truth && !p:
						d.fn++
					case p:
						d.fp++
					default:
						d.tn++
					}
				}
			}
			results = append(results, res)
		}
	}
	// ---- summary ----
	summary := map[string]any{"windows_per_day": perDay, "healthy_machines": *healthy, "days_per_healthy_machine": *days,
		"rtf_machines_per_fault": *rtf, "incipient_windows_unscored": incipient, "seed": *seed,
		"healthy_machine_days": healthyFleetDays(*healthy, *days, results),
		"dataset":              "SYNTHETIC (internal/hoistsim)", "alarm_rule": fmt.Sprintf("%d consecutive positive windows", k)}
	det := map[string]any{}
	fmt.Printf("windows/day %.0f; healthy fleet %d machines x %.0f days; run-to-failure %d machines x 4 faults\n\n",
		perDay, *healthy, *days, *rtf)
	fmt.Println("| detector | precision | recall | F1 | false alarms / machine-day (healthy fleet) | detected before failure | median lead time (days) | p10 lead time | median severity at detection |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|")
	for _, d := range dets {
		prec := float64(d.tp) / math.Max(1, float64(d.tp+d.fp))
		rec := float64(d.tp) / math.Max(1, float64(d.tp+d.fn))
		f1 := 2 * prec * rec / math.Max(1e-12, prec+rec)
		var leads, sevs []float64
		detected := 0
		for _, res := range results {
			if dd := res.DetectDay[d.name]; !math.IsNaN(dd) {
				detected++
				leads = append(leads, res.FailureDay-dd)
				sevs = append(sevs, res.DetectSev[d.name])
			}
		}
		sort.Float64s(leads)
		sort.Float64s(sevs)
		q := func(xs []float64, p float64) float64 {
			if len(xs) == 0 {
				return math.NaN()
			}
			return xs[int(p*float64(len(xs)-1))]
		}
		fa := float64(falseAlarms[d.name]) / healthyFleetDays(*healthy, *days, results)
		perFault := map[string]any{}
		for _, f := range faults {
			name := hoistsim.ClassNames[f]
			var l []float64
			miss := 0
			for _, res := range results {
				if res.Fault != name {
					continue
				}
				if dd := res.DetectDay[d.name]; math.IsNaN(dd) {
					miss++
				} else {
					l = append(l, res.FailureDay-dd)
				}
			}
			sort.Float64s(l)
			perFault[name] = map[string]any{"missed": miss, "median_lead_days": nanNull(q(l, 0.5))}
		}
		det[d.name] = map[string]any{"tp": d.tp, "fp": d.fp, "fn": d.fn, "tn": d.tn, "precision": prec, "recall": rec, "f1": f1,
			"false_alarms": falseAlarms[d.name], "false_alarms_per_machine_day": fa, "detected": detected, "machines": len(results),
			"median_lead_days": nanNull(q(leads, 0.5)), "p10_lead_days": nanNull(q(leads, 0.1)),
			"median_detect_severity": nanNull(q(sevs, 0.5)), "per_fault": perFault}
		fmt.Printf("| %s | %.3f | %.3f | %.3f | %.4f | %d/%d | %.1f | %.1f | %.2f |\n", d.name, prec, rec, f1, fa, detected,
			len(results), q(leads, 0.5), q(leads, 0.1), q(sevs, 0.5))
	}
	fmt.Println("\nModel confusion matrix over scored windows (rows true, columns predicted):", hoistsim.ClassNames)
	for i := range conf {
		fmt.Println(hoistsim.ClassNames[i], conf[i])
	}
	summary["detectors"] = det
	summary["model_confusion"] = conf
	summary["classes"] = hoistsim.ClassNames
	if *out != "" {
		os.MkdirAll(filepath.Dir(*out), 0o755)
		b, _ := json.MarshalIndent(summary, "", " ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

// healthyFleetDays: healthy-fleet machine-days plus the healthy period
// (before degradation starts) of the run-to-failure machines.
func healthyFleetDays(n int, days float64, results []rtfResult) float64 {
	t := float64(n) * days
	for _, r := range results {
		t += r.StartDay
	}
	return t
}

func nanNull(v float64) any {
	if math.IsNaN(v) {
		return nil
	}
	return v
}
