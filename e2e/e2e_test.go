// End-to-end tests: real edgegw binary, simulated PLC, real mosquitto.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"industrial-edge/internal/gateway"
	"industrial-edge/internal/mqtt"
	"industrial-edge/internal/plcsim"
	"industrial-edge/internal/queue"
	"industrial-edge/internal/testutil"
)

var binPath string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "edgegw")
	binPath = filepath.Join(dir, "edgegw")
	out, err := exec.Command("go", "build", "-o", binPath, "../cmd/edgegw").CombinedOutput()
	if err != nil {
		fmt.Println(string(out))
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type sink struct {
	mu   sync.Mutex
	seen map[uint64]gateway.Record
	dups int
	msgs int
	stop chan struct{}
}

// subscribe keeps a persistent-session subscriber connected (reconnecting
// across broker restarts) and records every record it receives.
func subscribe(t *testing.T, addr, topic string) *sink {
	s := &sink{seen: map[uint64]gateway.Record{}, stop: make(chan struct{})}
	ready := make(chan struct{})
	go func() {
		first := true
		for {
			select {
			case <-s.stop:
				return
			default:
			}
			c, err := mqtt.Connect(mqtt.Options{Addr: addr, ClientID: "e2e-sub", KeepAlive: 5 * time.Second, CleanSession: false,
				OnMessage: func(m mqtt.Message) {
					var recs []gateway.Record
					if err := json.Unmarshal(m.Payload, &recs); err != nil {
						t.Errorf("bad payload: %v", err)
						return
					}
					s.mu.Lock()
					s.msgs++
					for _, r := range recs {
						if _, dup := s.seen[r.Seq]; dup {
							s.dups++
						}
						s.seen[r.Seq] = r
					}
					s.mu.Unlock()
				}})
			if err != nil {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if err := c.Subscribe(topic, 1); err != nil {
				c.Close()
				continue
			}
			if first {
				close(ready)
				first = false
			}
			select {
			case <-c.Closed():
			case <-s.stop:
				c.Close()
				return
			}
		}
	}()
	<-ready
	return s
}

func writeConfig(t *testing.T, dir, plc, broker string, port int) string {
	b, _ := os.ReadFile("../configs/example.json")
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	cfg["data_dir"] = filepath.Join(dir, "data")
	cfg["http"] = fmt.Sprintf("127.0.0.1:%d", port)
	cfg["mqtt"].(map[string]any)["addr"] = broker
	cfg["mqtt"].(map[string]any)["keepalive_s"] = 2
	dev := cfg["devices"].([]any)[0].(map[string]any)
	dev["addr"] = plc
	dev["poll_ms"] = 20
	p := filepath.Join(dir, "gw.json")
	out, _ := json.Marshal(cfg)
	os.WriteFile(p, out, 0o644)
	return p
}

func startGW(t *testing.T, cfg string) *exec.Cmd {
	cmd := exec.Command(binPath, "-config", cfg, "-drain", "10s")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func finalOffset(t *testing.T, dir string) (uint64, uint64) {
	q, err := queue.Open(filepath.Join(dir, "data", "queue"), queue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	return q.NextOffset(), q.Backlog()
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNoLossAcrossBrokerOutageAndGatewayKill(t *testing.T) {
	dir := t.TempDir()
	plant := plcsim.New(1, 200)
	plc, err := plant.Listen("127.0.0.1:0", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer plant.Close()
	b := testutil.StartBroker(t)
	sub := subscribe(t, b.Addr, "plant1/gw1/data")
	defer close(sub.stop)
	port := testutil.FreePort(t)
	cfg := writeConfig(t, dir, plc, b.Addr, port)

	gw := startGW(t, cfg)
	time.Sleep(1500 * time.Millisecond)

	// 1. broker outage: the gateway keeps acquiring into its queue
	b.Stop()
	time.Sleep(2 * time.Second)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		t.Fatal(err)
	}
	var mb strings.Builder
	buf := make([]byte, 1<<16)
	for {
		n, err := resp.Body.Read(buf)
		mb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	resp.Body.Close()
	if !strings.Contains(mb.String(), "edge_uplink_connected 0") {
		t.Fatalf("uplink should report disconnected during the outage:\n%s", mb.String())
	}
	b.Restart(t)
	time.Sleep(1500 * time.Millisecond)

	// 2. crash the gateway (SIGKILL: no flush, no drain)
	gw.Process.Kill()
	gw.Wait()
	time.Sleep(300 * time.Millisecond)
	gw = startGW(t, cfg)
	time.Sleep(1500 * time.Millisecond)

	// 3. graceful stop while the broker is down: the gateway keeps
	// reconnecting during its drain window and empties the queue once the
	// broker returns
	b.Stop()
	time.Sleep(300 * time.Millisecond)
	gw.Process.Signal(syscall.SIGTERM)
	time.Sleep(time.Second)
	b.Restart(t)
	if err := gw.Wait(); err != nil {
		t.Fatalf("gateway exit: %v", err)
	}
	next, backlog := finalOffset(t, dir)
	if backlog != 0 {
		t.Fatalf("backlog %d after a graceful drain", backlog)
	}
	waitFor(t, 10*time.Second, "all records at the subscriber", func() bool {
		sub.mu.Lock()
		defer sub.mu.Unlock()
		return uint64(len(sub.seen)) >= next
	})
	sub.mu.Lock()
	defer sub.mu.Unlock()
	for s := uint64(0); s < next; s++ {
		if _, ok := sub.seen[s]; !ok {
			t.Fatalf("record %d of %d never delivered", s, next)
		}
	}
	// the counter tag is not compressed: its values must increase with seq
	var seqs []uint64
	for s, r := range sub.seen {
		if r.Tag == "counter" && r.Kind == "sample" {
			seqs = append(seqs, s)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i := 1; i < len(seqs); i++ {
		if sub.seen[seqs[i]].V < sub.seen[seqs[i-1]].V {
			t.Fatalf("counter went backwards at seq %d", seqs[i])
		}
	}
	kinds := map[string]int{}
	for _, r := range sub.seen {
		kinds[r.Kind]++
	}
	if kinds["anomaly"] == 0 || len(seqs) < 100 {
		t.Fatalf("expected anomalies from injected spikes and many counter samples: %v", kinds)
	}
	t.Logf("records %d, duplicates %d, batches %d, kinds %v", next, sub.dups, sub.msgs, kinds)
}

func TestPLCOutageReportsBadThenGoodQuality(t *testing.T) {
	dir := t.TempDir()
	plant := plcsim.New(2, 0)
	plc, _ := plant.Listen("127.0.0.1:0", 50)
	b := testutil.StartBroker(t)
	sub := subscribe(t, b.Addr, "plant1/gw1/data")
	defer close(sub.stop)
	port := testutil.FreePort(t)
	gw := startGW(t, writeConfig(t, dir, plc, b.Addr, port))
	time.Sleep(time.Second)
	plant.Close()
	time.Sleep(1500 * time.Millisecond)
	hr, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil || hr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("healthz should fail while the PLC is down: %v %v", hr.StatusCode, err)
	}
	plant2 := plcsim.New(3, 0)
	if _, err := plant2.Listen(plc, 50); err != nil {
		t.Fatal(err)
	}
	defer plant2.Close()
	time.Sleep(1500 * time.Millisecond)
	gw.Process.Signal(syscall.SIGTERM)
	gw.Wait()
	waitFor(t, 5*time.Second, "quality transitions", func() bool {
		sub.mu.Lock()
		defer sub.mu.Unlock()
		bad, good := 0, 0
		for _, r := range sub.seen {
			if r.Kind == "quality" && r.Quality == "bad" {
				bad++
			}
			if r.Kind == "quality" && r.Quality == "good" {
				good++
			}
		}
		return bad >= 6 && good >= 6 // every tag went bad and recovered
	})
}
