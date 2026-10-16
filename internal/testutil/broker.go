// Package testutil starts real external services for integration tests.
package testutil

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Broker is a mosquitto process on a free localhost port.
type Broker struct {
	Addr string
	cmd  *exec.Cmd
	conf string
	port int
}

// FreePort returns an unused TCP port.
func FreePort(t testing.TB) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// StartBroker starts mosquitto (skipping the test if it is not installed).
func StartBroker(t testing.TB) *Broker {
	bin, err := exec.LookPath("mosquitto")
	if err != nil {
		if _, e := os.Stat("/usr/sbin/mosquitto"); e == nil {
			bin = "/usr/sbin/mosquitto"
		} else {
			t.Skip("mosquitto not installed")
		}
	}
	b := &Broker{port: FreePort(t)}
	dir := t.TempDir()
	b.conf = filepath.Join(dir, "mosquitto.conf")
	// persistence on: subscriber sessions (clean_session=false) survive a
	// graceful broker restart, like a production broker
	os.WriteFile(b.conf, []byte(fmt.Sprintf("listener %d 127.0.0.1\nallow_anonymous true\npersistence true\npersistence_location %s/\n",
		b.port, dir)), 0o644)
	b.Addr = fmt.Sprintf("127.0.0.1:%d", b.port)
	b.start(t, bin)
	t.Cleanup(b.Stop)
	return b
}

func (b *Broker) start(t testing.TB, bin string) {
	b.cmd = exec.Command(bin, "-c", b.conf)
	if err := b.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", b.Addr); err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("mosquitto did not start")
}

// Stop shuts the broker down gracefully (it saves persistent sessions).
func (b *Broker) Stop() {
	if b.cmd != nil && b.cmd.Process != nil {
		b.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { b.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			b.cmd.Process.Kill()
			<-done
		}
		b.cmd = nil
	}
}

// Restart starts the broker again on the same port.
func (b *Broker) Restart(t testing.TB) {
	b.Stop()
	bin, _ := exec.LookPath("mosquitto")
	if bin == "" {
		bin = "/usr/sbin/mosquitto"
	}
	b.start(t, bin)
}
