package gateway

import (
	"bytes"
	"encoding/json"
	"log"
	"sync"
	"time"

	"industrial-edge/internal/mqtt"
)

// Uplink drains the queue to MQTT.
//
// It reads batches of up to BatchMax records after the last *sent* offset,
// sets each record's seq to its queue offset, and publishes the batch as one
// JSON array at QoS 1 on <prefix>/data. Up to Inflight batches may await
// their PUBACK; acknowledgements arrive in order on one connection, and the
// queue offset is committed (persisted) as each batch is acknowledged. On
// any connection error, in-flight batches are abandoned, the client
// reconnects with exponential back-off (100 ms .. 5 s), and sending restarts
// from the last committed offset — so records may be delivered twice but
// never lost.
type Uplink struct {
	g        *Gateway
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	conn     *mqtt.Client
	draining bool
	lastErr  string // last connect error (run goroutine only)
	// LastAckLagMs is now - timestamp of the newest record in the last acked batch.
	LastAckLagMs int64
}

// NewUplink creates (but does not start) the uplink.
func NewUplink(g *Gateway) *Uplink {
	return &Uplink{g: g, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start runs the uplink loop in a goroutine.
func (u *Uplink) Start() { go u.run() }

// Connected reports whether an MQTT session is up.
func (u *Uplink) Connected() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conn != nil
}

type inflight struct {
	ack    *mqtt.Ack
	end    uint64
	newest int64
}

func (u *Uplink) run() {
	defer close(u.done)
	backoff := 100 * time.Millisecond
	for {
		if u.finished() {
			return
		}
		c, err := mqtt.Connect(u.g.mqttOpts)
		if err != nil {
			if msg := err.Error(); msg != u.lastErr {
				log.Printf("uplink: connect: %v", err) // logged once per distinct error
				u.lastErr = msg
			}
			if !u.sleep(backoff) {
				return
			}
			if backoff *= 2; backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
			continue
		}
		backoff = 100 * time.Millisecond
		u.lastErr = ""
		u.g.Stats.Reconnects.Add(1)
		u.mu.Lock()
		u.conn = c
		u.mu.Unlock()
		err = u.session(c)
		u.mu.Lock()
		u.conn = nil
		u.mu.Unlock()
		c.Close()
		if err == nil {
			return // stopped
		}
		log.Printf("uplink: %v; reconnecting", err)
		u.g.Stats.PublishErrors.Add(1)
	}
}

// finished: stop was requested and there is nothing left to drain (or the
// drain timed out). While draining, the uplink keeps reconnecting.
func (u *Uplink) finished() bool {
	select {
	case <-u.stop:
		return !u.isDraining() || u.g.Q.Backlog() == 0
	default:
		return false
	}
}

func (u *Uplink) sleep(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if u.finished() {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// session publishes until the connection fails (error) or stop (nil).
func (u *Uplink) session(c *mqtt.Client) error {
	cfg := u.g.Cfg.MQTT
	q := u.g.Q
	next := q.AckedOffset()
	var window []inflight
	topic := cfg.TopicPrefix + "/data"
	for {
		// commit acknowledged batches, in order
		for len(window) > 0 {
			select {
			case <-window[0].ack.Done():
				if err := window[0].ack.Err(); err != nil {
					return err
				}
				if err := q.Ack(window[0].end); err != nil {
					return err
				}
				u.g.Stats.Published.Add(1)
				u.mu.Lock()
				u.LastAckLagMs = u.g.Now() - window[0].newest
				u.mu.Unlock()
				window = window[1:]
				continue
			default:
			}
			break
		}
		select {
		case <-u.stop:
			if !u.isDraining() || (len(window) == 0 && q.Backlog() == 0) {
				return nil
			}
		case <-c.Closed():
			return c.Err()
		default:
		}
		if next < q.AckedOffset() {
			next = q.AckedOffset() // records dropped by the disk cap
		}
		if len(window) >= cfg.Inflight {
			select {
			case <-window[0].ack.Done():
			case <-c.Closed():
				return c.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		recs, err := q.Read(next, cfg.BatchMax)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			select {
			case <-c.Closed():
				return c.Err()
			case <-time.After(20 * time.Millisecond):
			}
			if u.isDraining() && len(window) == 0 {
				select {
				case <-u.stop:
					return nil
				default:
				}
			}
			continue
		}
		var buf bytes.Buffer
		buf.WriteByte('[')
		var newest int64
		for i, r := range recs {
			var rec Record
			if err := json.Unmarshal(r.Payload, &rec); err != nil {
				log.Printf("uplink: skipping undecodable record %d: %v", r.Offset, err)
				continue
			}
			rec.Seq = r.Offset
			if rec.T > newest {
				newest = rec.T
			}
			b, _ := json.Marshal(rec)
			if i > 0 && buf.Len() > 1 {
				buf.WriteByte(',')
			}
			buf.Write(b)
		}
		buf.WriteByte(']')
		ack, err := c.Publish(topic, buf.Bytes(), 1, false)
		if err != nil {
			return err
		}
		end := recs[len(recs)-1].Offset + 1
		window = append(window, inflight{ack: ack, end: end, newest: newest})
		next = end
	}
}

func (u *Uplink) isDraining() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.draining
}

// StopAfterDrain stops the uplink once the queue is empty or after timeout.
func (u *Uplink) StopAfterDrain(timeout time.Duration) {
	u.mu.Lock()
	u.draining = true
	u.mu.Unlock()
	close(u.stop)
	select {
	case <-u.done:
	case <-time.After(timeout):
		u.mu.Lock()
		u.draining = false
		u.mu.Unlock()
		<-u.done
	}
}
