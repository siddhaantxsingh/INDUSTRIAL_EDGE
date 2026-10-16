package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	"industrial-edge/internal/mqtt"
)

// mqttOptions builds the MQTT connection options, loading TLS material and
// the password. It is called once at start-up so that bad files fail fast.
func mqttOptions(c MQTTConfig) (mqtt.Options, error) {
	o := mqtt.Options{Addr: c.Addr, ClientID: c.ClientID, KeepAlive: time.Duration(c.KeepAliveS) * time.Second,
		CleanSession: true, MaxInflight: c.Inflight, Username: c.Username}
	if c.Username != "" {
		switch {
		case c.PasswordFile != "":
			b, err := os.ReadFile(c.PasswordFile)
			if err != nil {
				return o, fmt.Errorf("mqtt password: %w", err)
			}
			o.Password = strings.TrimRight(string(b), "\r\n")
		default:
			o.Password = os.Getenv("EDGE_MQTT_PASSWORD")
		}
	}
	if t := c.TLS; t != nil {
		ca, err := os.ReadFile(t.CAFile)
		if err != nil {
			return o, fmt.Errorf("mqtt tls: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return o, fmt.Errorf("mqtt tls: no certificates in %s", t.CAFile)
		}
		cfg := &tls.Config{RootCAs: pool, ServerName: t.ServerName, MinVersion: tls.VersionTLS12}
		if t.CertFile != "" {
			cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
			if err != nil {
				return o, fmt.Errorf("mqtt tls client certificate: %w", err)
			}
			cfg.Certificates = []tls.Certificate{cert}
		}
		o.TLS = cfg
	}
	return o, nil
}
