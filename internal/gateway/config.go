package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// Config is the gateway configuration (JSON).
type Config struct {
	GatewayID string         `json:"gateway_id"`
	DataDir   string         `json:"data_dir"`
	HTTP      string         `json:"http"`
	MQTT      MQTTConfig     `json:"mqtt"`
	Queue     QueueConfig    `json:"queue"`
	Devices   []DeviceConfig `json:"devices"`
}

type MQTTConfig struct {
	Addr        string `json:"addr"`
	ClientID    string `json:"client_id"`
	TopicPrefix string `json:"topic_prefix"`
	KeepAliveS  int    `json:"keepalive_s"`
	BatchMax    int    `json:"batch_max"`
	Inflight    int    `json:"inflight"`
	// Security. TLS turns on MQTT over TLS (server verified against
	// ca_file; cert_file/key_file add a client certificate). Username plus
	// password_file (or the EDGE_MQTT_PASSWORD environment variable) send
	// credentials. require_tls refuses to start without TLS. Credentials
	// over plaintext are refused unless allow_plaintext_auth is set.
	TLS                *TLSConfig `json:"tls"`
	RequireTLS         bool       `json:"require_tls"`
	Username           string     `json:"username"`
	PasswordFile       string     `json:"password_file"`
	AllowPlaintextAuth bool       `json:"allow_plaintext_auth"`
}

// TLSConfig holds PEM file paths for MQTT over TLS.
type TLSConfig struct {
	CAFile     string `json:"ca_file"`
	CertFile   string `json:"cert_file"`
	KeyFile    string `json:"key_file"`
	ServerName string `json:"server_name"`
}

type QueueConfig struct {
	SegmentBytes int64 `json:"segment_bytes"`
	MaxBytes     int64 `json:"max_bytes"`
	SyncMs       int   `json:"sync_ms"`
}

type DeviceConfig struct {
	Name      string           `json:"name"`
	Addr      string           `json:"addr"`
	Unit      byte             `json:"unit"`
	PollMs    int              `json:"poll_ms"`
	TimeoutMs int              `json:"timeout_ms"`
	Tags      []TagConfig      `json:"tags"`
	Vibration *VibrationConfig `json:"vibration"`
}

type TagConfig struct {
	Name        string             `json:"name"`
	Table       string             `json:"table"` // holding | input | coil
	Addr        uint16             `json:"addr"`
	Type        string             `json:"type"` // uint16 int16 uint32 int32 float32 bool
	WordSwap    bool               `json:"word_swap"`
	Scale       float64            `json:"scale"`
	Offset      float64            `json:"offset"`
	Unit        string             `json:"unit"`
	Compression *CompressionConfig `json:"compression"`
	Alarm       *AlarmConfig       `json:"alarm"`
	Anomaly     *AnomalyConfig     `json:"anomaly"`
}

type CompressionConfig struct {
	Type     string  `json:"type"` // none | deadband | swinging_door
	Dev      float64 `json:"dev"`
	MaxGapMs int64   `json:"max_gap_ms"`
}

type AlarmConfig struct {
	High       float64 `json:"high"`
	Deadband   float64 `json:"deadband"`
	OnDelayMs  int64   `json:"on_delay_ms"`
	OffDelayMs int64   `json:"off_delay_ms"`
}

type AnomalyConfig struct {
	Alpha  float64 `json:"alpha"`
	Z      float64 `json:"z"`
	Warmup int     `json:"warmup"`
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// regs returns how many 16-bit registers a type occupies.
func regs(typ string) (uint16, error) {
	switch typ {
	case "uint16", "int16", "bool":
		return 1, nil
	case "uint32", "int32", "float32":
		return 2, nil
	}
	return 0, fmt.Errorf("unknown type %q", typ)
}

// LoadConfig reads and validates a config file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytesReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, c.Validate()
}

// Validate fills defaults and rejects inconsistent configurations.
func (c *Config) Validate() error {
	if !nameRE.MatchString(c.GatewayID) {
		return fmt.Errorf("config: gateway_id %q invalid", c.GatewayID)
	}
	if c.DataDir == "" {
		return fmt.Errorf("config: data_dir required")
	}
	if c.MQTT.Addr == "" || c.MQTT.TopicPrefix == "" {
		return fmt.Errorf("config: mqtt.addr and mqtt.topic_prefix required")
	}
	if c.MQTT.RequireTLS && c.MQTT.TLS == nil {
		return fmt.Errorf("config: mqtt.require_tls is set but mqtt.tls is missing: refusing plaintext MQTT")
	}
	if c.MQTT.TLS != nil && c.MQTT.TLS.CAFile == "" {
		return fmt.Errorf("config: mqtt.tls.ca_file required (the broker certificate must be verified)")
	}
	if c.MQTT.TLS != nil && (c.MQTT.TLS.CertFile == "") != (c.MQTT.TLS.KeyFile == "") {
		return fmt.Errorf("config: mqtt.tls.cert_file and key_file go together")
	}
	if c.MQTT.Username != "" && c.MQTT.TLS == nil && !c.MQTT.AllowPlaintextAuth {
		return fmt.Errorf("config: mqtt.username without mqtt.tls would send the password in clear text " +
			"(set allow_plaintext_auth only for local testing)")
	}
	if c.MQTT.ClientID == "" {
		c.MQTT.ClientID = c.GatewayID
	}
	if c.MQTT.KeepAliveS == 0 {
		c.MQTT.KeepAliveS = 10
	}
	if c.MQTT.BatchMax == 0 {
		c.MQTT.BatchMax = 500
	}
	if c.MQTT.Inflight == 0 {
		c.MQTT.Inflight = 8
	}
	if c.Queue.SyncMs == 0 {
		c.Queue.SyncMs = 100
	}
	if len(c.Devices) == 0 {
		return fmt.Errorf("config: no devices")
	}
	seen := map[string]bool{}
	for di := range c.Devices {
		d := &c.Devices[di]
		if !nameRE.MatchString(d.Name) || seen[d.Name] {
			return fmt.Errorf("config: device name %q invalid or duplicate", d.Name)
		}
		seen[d.Name] = true
		if d.PollMs <= 0 {
			d.PollMs = 1000
		}
		if d.TimeoutMs <= 0 {
			d.TimeoutMs = 1000
		}
		if d.Unit == 0 {
			d.Unit = 1
		}
		tags := map[string]bool{}
		if d.Vibration != nil {
			if err := d.Vibration.validate(d.Name); err != nil {
				return err
			}
			tags["fault"] = true // reserved pseudo-tag
		}
		for ti := range d.Tags {
			t := &d.Tags[ti]
			if !nameRE.MatchString(t.Name) || tags[t.Name] {
				return fmt.Errorf("config: tag %s.%q invalid or duplicate", d.Name, t.Name)
			}
			tags[t.Name] = true
			if _, err := regs(t.Type); err != nil {
				return fmt.Errorf("config: tag %s.%s: %w", d.Name, t.Name, err)
			}
			switch t.Table {
			case "holding", "input":
				if t.Type == "bool" {
					return fmt.Errorf("config: tag %s.%s: bool needs table coil", d.Name, t.Name)
				}
			case "coil":
				if t.Type != "bool" {
					return fmt.Errorf("config: tag %s.%s: coil tags must be bool", d.Name, t.Name)
				}
			default:
				return fmt.Errorf("config: tag %s.%s: table %q", d.Name, t.Name, t.Table)
			}
			if t.Scale == 0 {
				t.Scale = 1
			}
			if t.Compression != nil {
				switch t.Compression.Type {
				case "none", "deadband", "swinging_door":
				default:
					return fmt.Errorf("config: tag %s.%s: compression %q", d.Name, t.Name, t.Compression.Type)
				}
				if t.Compression.Type != "none" && t.Compression.Dev < 0 {
					return fmt.Errorf("config: tag %s.%s: negative deviation", d.Name, t.Name)
				}
			}
		}
	}
	return nil
}
