package mqtt

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"industrial-edge/internal/testutil"
)

func TestPublishSubscribeQoS1(t *testing.T) {
	b := testutil.StartBroker(t)
	var mu sync.Mutex
	got := map[string]bool{}
	sub, err := Connect(Options{Addr: b.Addr, ClientID: "sub", KeepAlive: 10 * time.Second, CleanSession: true,
		OnMessage: func(m Message) { mu.Lock(); got[string(m.Payload)] = true; mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if err := sub.Subscribe("plant/#", 1); err != nil {
		t.Fatal(err)
	}
	pub, err := Connect(Options{Addr: b.Addr, ClientID: "pub", KeepAlive: 10 * time.Second, CleanSession: true, MaxInflight: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	var acks []*Ack
	for i := 0; i < 500; i++ {
		a, err := pub.Publish(fmt.Sprintf("plant/line%d", i%3), []byte(fmt.Sprint(i)), 1, false)
		if err != nil {
			t.Fatal(err)
		}
		acks = append(acks, a)
	}
	for _, a := range acks {
		select {
		case <-a.Done():
			if a.Err() != nil {
				t.Fatal(a.Err())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("PUBACK timeout")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 500 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscriber got %d/500", len(got))
}

func TestBrokerDeathFailsPendingAcks(t *testing.T) {
	b := testutil.StartBroker(t)
	c, err := Connect(Options{Addr: b.Addr, ClientID: "x", KeepAlive: 2 * time.Second, CleanSession: true})
	if err != nil {
		t.Fatal(err)
	}
	b.Stop()
	select {
	case <-c.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("connection loss not detected")
	}
	if _, err := c.Publish("a", []byte("b"), 1, false); err == nil {
		t.Fatal("publish on a dead connection succeeded")
	}
}

func TestRemainingLengthEncoding(t *testing.T) {
	for _, n := range []int{0, 127, 128, 16383, 16384, 2097151, 2097152, maxRemain} {
		enc := encodeRemaining(n)
		v, m := 0, 1
		for _, b := range enc {
			v += int(b&0x7f) * m
			m *= 128
		}
		if v != n || len(enc) > 4 {
			t.Fatalf("%d -> %x", n, enc)
		}
	}
}
