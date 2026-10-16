// Package mqtt is a minimal MQTT 3.1.1 client written from the OASIS
// specification: CONNECT/CONNACK, PUBLISH at QoS 0 and 1 with PUBACK
// tracking, SUBSCRIBE/SUBACK (used by tests and the bench subscriber),
// PINGREQ keepalive and DISCONNECT. QoS 1 publishes return a channel that
// is closed when the broker acknowledges the message, which is what the
// store-and-forward uplink uses to commit its queue offset.
//
// The client does not reconnect by itself: on any error it closes, every
// pending acknowledgement fails, and the owner creates a new connection
// (the uplink then re-sends everything not yet acknowledged).
package mqtt

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	pConnect    = 1
	pConnack    = 2
	pPublish    = 3
	pPuback     = 4
	pSubscribe  = 8
	pSuback     = 9
	pPingreq    = 12
	pPingresp   = 13
	pDisconnect = 14
	maxRemain   = 268435455
)

// ErrClosed is returned for operations on a closed client.
var ErrClosed = errors.New("mqtt: connection closed")

// Message is a received PUBLISH.
type Message struct {
	Topic   string
	Payload []byte
	QoS     byte
	Dup     bool
}

// Options configures a connection.
type Options struct {
	Addr         string
	ClientID     string
	KeepAlive    time.Duration
	CleanSession bool
	DialTimeout  time.Duration
	// MaxInflight bounds unacknowledged QoS 1 publishes (0 = 64).
	MaxInflight int
	OnMessage   func(Message)
	// TLS, when set, makes the connection TLS (MQTT over TLS, usually port
	// 8883). The server certificate is verified against TLS.RootCAs (or the
	// system pool); a client certificate in TLS.Certificates is presented
	// when the broker asks for one.
	TLS *tls.Config
	// Username/Password are sent in CONNECT when Username is non-empty.
	Username string
	Password string
}

// ConnackError is a CONNACK with a non-zero return code.
type ConnackError struct{ Code byte }

func (e *ConnackError) Error() string {
	msg := map[byte]string{1: "unacceptable protocol version", 2: "identifier rejected", 3: "server unavailable",
		4: "bad user name or password", 5: "not authorized"}[e.Code]
	return fmt.Sprintf("mqtt: connection refused, code %d (%s)", e.Code, msg)
}

// Ack resolves when a QoS 1 publish is acknowledged (nil) or the connection dies (error).
type Ack struct {
	done chan struct{}
	err  error
}

// Done is closed when the outcome is known.
func (a *Ack) Done() <-chan struct{} { return a.done }

// Err returns the outcome after Done is closed.
func (a *Ack) Err() error { return a.err }

// Client is one MQTT connection.
type Client struct {
	opt     Options
	conn    net.Conn
	w       *bufio.Writer
	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  uint16
	pending map[uint16]*Ack
	subacks map[uint16]chan []byte
	slots   chan struct{}
	closed  chan struct{}
	once    sync.Once
	err     error
}

// Connect dials the broker and completes the CONNECT handshake.
func Connect(o Options) (*Client, error) {
	if o.DialTimeout == 0 {
		o.DialTimeout = 5 * time.Second
	}
	if o.MaxInflight == 0 {
		o.MaxInflight = 64
	}
	var conn net.Conn
	var err error
	if o.TLS != nil {
		d := &net.Dialer{Timeout: o.DialTimeout}
		cfg := o.TLS.Clone()
		if cfg.MinVersion == 0 {
			cfg.MinVersion = tls.VersionTLS12
		}
		if cfg.ServerName == "" {
			if host, _, e := net.SplitHostPort(o.Addr); e == nil {
				cfg.ServerName = host
			}
		}
		tc, e := tls.DialWithDialer(d, "tcp", o.Addr, cfg)
		if e != nil {
			return nil, fmt.Errorf("mqtt: tls: %w", e)
		}
		conn, err = tc, nil
	} else {
		conn, err = net.DialTimeout("tcp", o.Addr, o.DialTimeout)
	}
	if err != nil {
		return nil, err
	}
	c := &Client{opt: o, conn: conn, w: bufio.NewWriterSize(conn, 64<<10), pending: map[uint16]*Ack{},
		subacks: map[uint16]chan []byte{}, slots: make(chan struct{}, o.MaxInflight), closed: make(chan struct{})}
	// CONNECT
	var vh []byte
	vh = appendString(vh, "MQTT")
	flags := byte(0)
	if o.CleanSession {
		flags |= 0x02
	}
	if o.Username != "" {
		flags |= 0x80
		if o.Password != "" {
			flags |= 0x40
		}
	}
	vh = append(vh, 4, flags, 0, 0)
	binary.BigEndian.PutUint16(vh[len(vh)-2:], uint16(o.KeepAlive/time.Second))
	vh = appendString(vh, o.ClientID)
	if o.Username != "" {
		vh = appendString(vh, o.Username)
		if o.Password != "" {
			vh = appendString(vh, o.Password)
		}
	}
	if err := c.writePacket(pConnect<<4, vh); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(o.DialTimeout))
	br := bufio.NewReader(conn)
	typ, body, err := readPacket(br)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if typ>>4 != pConnack || len(body) != 2 {
		conn.Close()
		return nil, fmt.Errorf("mqtt: expected CONNACK, got type %d", typ>>4)
	}
	if body[1] != 0 {
		conn.Close()
		return nil, &ConnackError{Code: body[1]}
	}
	_ = conn.SetReadDeadline(time.Time{})
	go c.readLoop(br)
	if o.KeepAlive > 0 {
		go c.pingLoop()
	}
	return c, nil
}

func appendString(b []byte, s string) []byte {
	b = append(b, byte(len(s)>>8), byte(len(s)))
	return append(b, s...)
}

func encodeRemaining(n int) []byte {
	var out []byte
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		out = append(out, d)
		if n == 0 {
			return out
		}
	}
}

func readPacket(r *bufio.Reader) (byte, []byte, error) {
	typ, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, mult := 0, 1
	for i := 0; ; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n += int(b&0x7f) * mult
		if b&0x80 == 0 {
			break
		}
		mult *= 128
		if i == 3 {
			return 0, nil, errors.New("mqtt: malformed remaining length")
		}
	}
	if n > maxRemain {
		return 0, nil, errors.New("mqtt: packet too large")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

func (c *Client) writePacket(hdr byte, body []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.w.WriteByte(hdr); err != nil {
		return err
	}
	if _, err := c.w.Write(encodeRemaining(len(body))); err != nil {
		return err
	}
	if _, err := c.w.Write(body); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *Client) fail(err error) {
	c.once.Do(func() {
		c.err = err
		close(c.closed)
		_ = c.conn.Close()
		c.mu.Lock()
		for id, a := range c.pending {
			a.err = err
			close(a.done)
			delete(c.pending, id)
		}
		for id, ch := range c.subacks {
			close(ch)
			delete(c.subacks, id)
		}
		c.mu.Unlock()
	})
}

func (c *Client) readLoop(r *bufio.Reader) {
	for {
		if c.opt.KeepAlive > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(c.opt.KeepAlive * 3 / 2))
		}
		typ, body, err := readPacket(r)
		if err != nil {
			c.fail(fmt.Errorf("mqtt: read: %w", err))
			return
		}
		switch typ >> 4 {
		case pPuback:
			if len(body) != 2 {
				c.fail(errors.New("mqtt: malformed PUBACK"))
				return
			}
			id := binary.BigEndian.Uint16(body)
			c.mu.Lock()
			a := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if a != nil {
				close(a.done)
				<-c.slots
			}
		case pSuback:
			if len(body) < 3 {
				c.fail(errors.New("mqtt: malformed SUBACK"))
				return
			}
			id := binary.BigEndian.Uint16(body)
			c.mu.Lock()
			ch := c.subacks[id]
			delete(c.subacks, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- body[2:]
			}
		case pPublish:
			qos := (typ >> 1) & 3
			if len(body) < 2 {
				c.fail(errors.New("mqtt: malformed PUBLISH"))
				return
			}
			tl := int(binary.BigEndian.Uint16(body))
			if 2+tl > len(body) {
				c.fail(errors.New("mqtt: malformed PUBLISH topic"))
				return
			}
			topic := string(body[2 : 2+tl])
			rest := body[2+tl:]
			if qos > 0 {
				if len(rest) < 2 {
					c.fail(errors.New("mqtt: malformed PUBLISH id"))
					return
				}
				id := rest[:2]
				rest = rest[2:]
				_ = c.writePacket(pPuback<<4, append([]byte(nil), id...))
			}
			if c.opt.OnMessage != nil {
				c.opt.OnMessage(Message{Topic: topic, Payload: rest, QoS: qos, Dup: typ&0x08 != 0})
			}
		case pPingresp:
		default:
			c.fail(fmt.Errorf("mqtt: unexpected packet type %d", typ>>4))
			return
		}
	}
}

func (c *Client) pingLoop() {
	t := time.NewTicker(c.opt.KeepAlive / 2)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			if err := c.writePacket(pPingreq<<4, nil); err != nil {
				c.fail(err)
				return
			}
		}
	}
}

func (c *Client) id() uint16 {
	for {
		c.nextID++
		if c.nextID == 0 {
			continue
		}
		if _, used := c.pending[c.nextID]; !used {
			if _, used := c.subacks[c.nextID]; !used {
				return c.nextID
			}
		}
	}
}

// Publish sends a message. For QoS 1 it blocks while MaxInflight messages
// are unacknowledged and returns an Ack; for QoS 0 the Ack is already done.
func (c *Client) Publish(topic string, payload []byte, qos byte, retain bool) (*Ack, error) {
	a := &Ack{done: make(chan struct{})}
	hdr := byte(pPublish<<4) | qos<<1
	if retain {
		hdr |= 1
	}
	body := appendString(make([]byte, 0, 4+len(topic)+len(payload)), topic)
	if qos == 1 {
		select {
		case c.slots <- struct{}{}:
		case <-c.closed:
			return nil, ErrClosed
		}
		c.mu.Lock()
		select {
		case <-c.closed:
			c.mu.Unlock()
			return nil, ErrClosed
		default:
		}
		id := c.id()
		c.pending[id] = a
		c.mu.Unlock()
		body = append(body, byte(id>>8), byte(id))
	} else if qos != 0 {
		return nil, errors.New("mqtt: only QoS 0 and 1 are supported")
	}
	body = append(body, payload...)
	if err := c.writePacket(hdr, body); err != nil {
		c.fail(err)
		return nil, err
	}
	if qos == 0 {
		close(a.done)
	}
	return a, nil
}

// Subscribe subscribes to one topic filter and waits for SUBACK.
func (c *Client) Subscribe(filter string, qos byte) error {
	c.mu.Lock()
	id := c.id()
	ch := make(chan []byte, 1)
	c.subacks[id] = ch
	c.mu.Unlock()
	body := []byte{byte(id >> 8), byte(id)}
	body = appendString(body, filter)
	body = append(body, qos)
	if err := c.writePacket(pSubscribe<<4|0x02, body); err != nil {
		c.fail(err)
		return err
	}
	select {
	case codes, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if len(codes) != 1 || codes[0] == 0x80 {
			return fmt.Errorf("mqtt: subscription to %q refused", filter)
		}
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("mqtt: SUBACK timeout")
	}
}

// Closed is closed when the connection has failed or been closed.
func (c *Client) Closed() <-chan struct{} { return c.closed }

// Err is the reason the connection closed.
func (c *Client) Err() error { return c.err }

// Close sends DISCONNECT and closes the connection.
func (c *Client) Close() error {
	_ = c.writePacket(pDisconnect<<4, nil)
	c.fail(ErrClosed)
	return nil
}
