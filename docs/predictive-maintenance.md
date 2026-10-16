# Predictive-maintenance evaluation (simulated run-to-failure)

`go run ./cmd/pdmeval -healthy 20 -days 30 -rtf 20 -out bench/results/2026-10-14/pdm.json` (log: `docs/evidence/2026-10-14/pdmeval.log`; shared 4-vCPU machine, load average 30–44 during the run).

**All data is synthetic** (`internal/hoistsim`, seed 11, different from the training seed). The results show how the detectors behave on the simulator, not on real hoists.

## Set-up

* One vibration window every 10 minutes (144 per day). Speeds 600–1750 rpm, including the band held out from training.
* Healthy fleet: 20 machines × 30 days, plus the healthy first 2–10 days of each run-to-failure machine (total 1,063 healthy machine-days; 116 false alarms) — used for false alarms.
* Run-to-failure: 20 machines for each of imbalance, misalignment, outer race and inner race (80 in total). Severity grows exponentially from 0 to 1.0 over 10–30 days; failure = severity 1.0.
* Window-level truth: faulty when severity ≥ 0.1 (the lowest training severity). Windows with 0 < severity < 0.1 are not scored.
* Same alarm rule for every detector: raise after 3 consecutive positive windows, clear after 3 negative ones (as in the gateway, `alarm_windows: 3`).
* Baseline: the gateway's existing EWMA z-score (alpha 0.05, warm-up 50) on the window's vibration RMS, at |z| ≥ 3 and ≥ 6.

## Results

| Detector | Precision | Recall | F1 | False alarms per machine-day | Machines alarmed before failure | Median lead time | 10th-percentile lead time |
|---|---|---|---|---|---|---|---|
| INT8 edge model | 0.919 | 0.926 | 0.923 | 0.109 | 80 / 80 | 11.2 days | 6.1 days |
| EWMA z ≥ 3 | 0.434 | 0.004 | 0.008 | 0 | 0 / 80 | — | — |
| EWMA z ≥ 6 | 0.000 | 0.000 | 0.000 | 0 | 0 / 80 | — | — |

Model confusion matrix over scored windows (rows true, columns predicted: healthy, imbalance, misalignment, outer, inner, overload): see `bench/results/2026-10-14/pdm.json`. 5.9 % of healthy windows are flagged as a fault (higher than the 3 % on the test set, because every machine has its own baseline and all speeds occur), and 16.5 % of inner-race windows are missed as healthy.

## How to read this

* **EWMA cannot see slow degradation.** It scores each sample against a moving average with a time constant of about 3 hours; a fault that grows over days is absorbed into the average. It is a spike detector, and it does what it was built for in the existing gateway (temperature spikes), but it is the wrong tool for wear.
* **The model's false-alarm rate is too high for production**: about one false alarm per machine every 9 days. Longer alarm delays, per-machine baselining or a confidence threshold would reduce it; none was tuned here.
* **Lead times are optimistic.** The median severity at the first alarm is 0.06, below the scored range. With 0.11 false alarms per day, some "early detections" in the first days of a degradation are partly chance. A stricter analysis would measure lead time only from alarms that stay raised.
* Overload is not part of the run-to-failure study (it is an operating event, not wear).
