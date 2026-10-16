package mqtt_test

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"industrial-edge/internal/mqtt"
	"industrial-edge/internal/testutil"
)

func tlsConf(t *testing.T, ca, cert, key string) *tls.Config {
	pem, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	c := &tls.Config{RootCAs: pool}
	if cert != "" {
		kp, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			t.Fatal(err)
		}
		c.Certificates = []tls.Certificate{kp}
	}
	return c
}

func TestTLSWithClientCertAndPassword(t *testing.T) {
	b := testutil.StartSecureBroker(t)
	p := b.PKI
	got := make(chan string, 1)
	sub, err := mqtt.Connect(mqtt.Options{Addr: b.Addr, ClientID: "sub", TLS: tlsConf(t, p.CA, p.ClientCert, p.ClientKey),
		Username: b.User, Password: b.Password, OnMessage: func(m mqtt.Message) { got <- string(m.Payload) }})
	if err != nil {
		t.Fatalf("valid cert + password refused: %v", err)
	}
	defer sub.Close()
	if err := sub.Subscribe("secure/#", 1); err != nil {
		t.Fatal(err)
	}
	pub, err := mqtt.Connect(mqtt.Options{Addr: b.Addr, ClientID: "pub", TLS: tlsConf(t, p.CA, p.ClientCert, p.ClientKey),
		Username: b.User, Password: b.Password})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	a, err := pub.Publish("secure/x", []byte("hello"), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	<-a.Done()
	if a.Err() != nil {
		t.Fatal(a.Err())
	}
	select {
	case m := <-got:
		if m != "hello" {
			t.Fatal(m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message not received over TLS")
	}
}

func TestTLSRefusals(t *testing.T) {
	b := testutil.StartSecureBroker(t)
	p := b.PKI
	cases := []struct {
		name string
		o    mqtt.Options
	}{
		{"wrong password", mqtt.Options{TLS: tlsConf(t, p.CA, p.ClientCert, p.ClientKey), Username: b.User, Password: "wrong"}},
		{"no credentials", mqtt.Options{TLS: tlsConf(t, p.CA, p.ClientCert, p.ClientKey)}},
		{"no client certificate", mqtt.Options{TLS: tlsConf(t, p.CA, "", ""), Username: b.User, Password: b.Password}},
		{"client certificate from an untrusted CA", mqtt.Options{TLS: tlsConf(t, p.CA, p.RogueClientCert, p.RogueClient),
			Username: b.User, Password: b.Password}},
		{"server not trusted by the client", mqtt.Options{TLS: tlsConf(t, p.RogueCA, p.ClientCert, p.ClientKey),
			Username: b.User, Password: b.Password}},
		{"plaintext client on the TLS port", mqtt.Options{Username: b.User, Password: b.Password}},
	}
	for _, c := range cases {
		c.o.Addr, c.o.ClientID, c.o.DialTimeout = b.Addr, "x", 3*time.Second
		cl, err := mqtt.Connect(c.o)
		if err == nil {
			// a TLS 1.3 server may reject the client certificate only after
			// the handshake: the first read then fails
			_, perr := cl.Publish("secure/x", []byte("x"), 1, false)
			select {
			case <-cl.Closed():
				err = cl.Err()
			case <-time.After(3 * time.Second):
				if perr == nil {
					t.Errorf("%s: connection accepted", c.name)
					cl.Close()
					continue
				}
				err = perr
			}
		}
		var ce *mqtt.ConnackError
		if (c.name == "wrong password" || c.name == "no credentials") && !errors.As(err, &ce) {
			t.Errorf("%s: want a CONNACK refusal, got %v", c.name, err)
		}
		if c.name == "server not trusted by the client" && !strings.Contains(fmt.Sprint(err), "x509") {
			t.Errorf("%s: want a certificate verification error, got %v", c.name, err)
		}
		t.Logf("%s: refused: %v (CONNACK code: %v)", c.name, err, errors.As(err, &ce))
	}
}
