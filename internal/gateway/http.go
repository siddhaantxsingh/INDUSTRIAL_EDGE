package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Handler serves /metrics (Prometheus text), /healthz, /tags and
// POST /alarms/ack?dev=..&tag=.. (operator acknowledgement).
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		c := func(name, help string, v uint64) {
			fmt.Fprintf(&b, "# HELP edge_%s %s\n# TYPE edge_%s counter\nedge_%s %d\n", name, help, name, name, v)
		}
		gauge := func(name, help string, v float64) {
			fmt.Fprintf(&b, "# HELP edge_%s %s\n# TYPE edge_%s gauge\nedge_%s %g\n", name, help, name, name, v)
		}
		s := &g.Stats
		c("polls_total", "device poll cycles", s.Polls.Load())
		c("poll_errors_total", "failed block reads", s.PollErrors.Load())
		c("modbus_requests_total", "Modbus requests issued", s.ModbusRequests.Load())
		c("samples_total", "tag samples acquired", s.Samples.Load())
		c("archived_total", "samples kept after compression", s.Archived.Load())
		c("events_total", "alarm and anomaly events", s.Events.Load())
		c("published_batches_total", "batches acknowledged by the broker", s.Published.Load())
		c("uplink_errors_total", "uplink connection failures", s.PublishErrors.Load())
		c("mqtt_connects_total", "successful MQTT connections", s.Reconnects.Load())
		c("queue_dropped_total", "records dropped unacknowledged by the disk cap", g.Q.Dropped)
		c("inferences_total", "edge fault-classifier inferences", s.Inferences.Load())
		fmt.Fprintf(&b, "# HELP edge_inference_seconds_total time spent in edge inference (features + INT8 MLP)\n"+
			"# TYPE edge_inference_seconds_total counter\nedge_inference_seconds_total %g\n", float64(s.InferenceNanos.Load())/1e9)
		c("inference_torn_total", "vibration windows discarded because they changed during the read", s.InferenceTorn.Load())
		c("drift_alerts_total", "input (PSI) and confidence (Page-Hinkley) drift alerts", s.DriftAlerts.Load())
		gauge("queue_backlog", "unacknowledged records", float64(g.Q.Backlog()))
		gauge("queue_disk_bytes", "bytes in queue segments", float64(g.Q.DiskBytes()))
		up := 0.0
		lag := 0.0
		if g.uplink != nil {
			if g.uplink.Connected() {
				up = 1
			}
			g.uplink.mu.Lock()
			lag = float64(g.uplink.LastAckLagMs) / 1000
			g.uplink.mu.Unlock()
		}
		gauge("uplink_connected", "1 when an MQTT session is up", up)
		gauge("uplink_ack_lag_seconds", "age of the newest record in the last acknowledged batch", lag)
		if n := s.Samples.Load(); n > 0 {
			gauge("compression_ratio", "acquired samples per archived sample", float64(n)/float64(max(1, s.Archived.Load())))
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.Write([]byte(b.String()))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ok, msg := g.Healthy()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		w.Write([]byte(msg + "\n"))
	})
	mux.HandleFunc("/tags", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(g.Latest())
	})
	mux.HandleFunc("/alarms/ack", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if err := g.AckAlarm(r.URL.Query().Get("dev"), r.URL.Query().Get("tag")); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
