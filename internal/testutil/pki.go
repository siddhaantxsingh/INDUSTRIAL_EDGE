package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// PKI is a throw-away certificate authority with server and client
// certificates, plus a second, untrusted CA with its own client certificate.
type PKI struct {
	Dir                                   string
	CA, ServerCert, ServerKey             string
	ClientCert, ClientKey                 string
	RogueCA, RogueClientCert, RogueClient string
}

type issued struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func writePEM(t testing.TB, path, typ string, der []byte) {
	// 0644 so that mosquitto can read its key after dropping privileges (tests only)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}

func issue(t testing.TB, dir, name string, tmpl *x509.Certificate, parent *issued) issued {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(24 * time.Hour)
	signer, signKey := tmpl, key
	if parent != nil {
		signer, signKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	writePEM(t, filepath.Join(dir, name+".pem"), "CERTIFICATE", der)
	kb, _ := x509.MarshalECPrivateKey(key)
	writePEM(t, filepath.Join(dir, name+".key"), "EC PRIVATE KEY", kb)
	return issued{cert, key}
}

// NewPKI creates the certificates in a temporary directory.
func NewPKI(t testing.TB) *PKI {
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	caT := &x509.Certificate{Subject: pkix.Name{CommonName: "edge test CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	ca := issue(t, dir, "ca", caT, nil)
	issue(t, dir, "server", &x509.Certificate{Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage: x509.KeyUsageDigitalSignature}, &ca)
	issue(t, dir, "client", &x509.Certificate{Subject: pkix.Name{CommonName: "gw1"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}, &ca)
	rogue := issue(t, dir, "rogue-ca", &x509.Certificate{Subject: pkix.Name{CommonName: "rogue CA"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}, nil)
	issue(t, dir, "rogue-client", &x509.Certificate{Subject: pkix.Name{CommonName: "gw1"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}, &rogue)
	p := func(n string) string { return filepath.Join(dir, n) }
	return &PKI{Dir: dir, CA: p("ca.pem"), ServerCert: p("server.pem"), ServerKey: p("server.key"),
		ClientCert: p("client.pem"), ClientKey: p("client.key"), RogueCA: p("rogue-ca.pem"),
		RogueClientCert: p("rogue-client.pem"), RogueClient: p("rogue-client.key")}
}

// SecureBroker is mosquitto with a TLS-only listener that requires a client
// certificate signed by the PKI's CA and a user name/password.
type SecureBroker struct {
	*Broker
	PKI            *PKI
	User, Password string
}

// StartSecureBroker starts it (skipping if mosquitto/mosquitto_passwd are missing).
func StartSecureBroker(t testing.TB) *SecureBroker {
	bin := mosquittoBin(t)
	pw, err := exec.LookPath("mosquitto_passwd")
	if err != nil {
		t.Skip("mosquitto_passwd not installed")
	}
	pki := NewPKI(t)
	sb := &SecureBroker{PKI: pki, User: "gw1", Password: "s3cret-edge"}
	passwd := filepath.Join(pki.Dir, "passwd")
	if out, err := exec.Command(pw, "-b", "-c", passwd, sb.User, sb.Password).CombinedOutput(); err != nil {
		t.Fatalf("mosquitto_passwd: %v %s", err, out)
	}
	os.Chmod(passwd, 0o644)
	b := &Broker{port: FreePort(t)}
	b.conf = filepath.Join(pki.Dir, "mosquitto.conf")
	conf := fmt.Sprintf("listener %d 127.0.0.1\ncafile %s\ncertfile %s\nkeyfile %s\nrequire_certificate true\n"+
		"tls_version tlsv1.2\nallow_anonymous false\npassword_file %s\n", b.port, pki.CA, pki.ServerCert, pki.ServerKey, passwd)
	if os.Geteuid() == 0 {
		conf += "user root\n" // do not drop privileges: the files live in a private temp dir
	}
	os.WriteFile(b.conf, []byte(conf), 0o644)
	b.Addr = fmt.Sprintf("127.0.0.1:%d", b.port)
	b.start(t, bin)
	t.Cleanup(b.Stop)
	sb.Broker = b
	return sb
}

func mosquittoBin(t testing.TB) string {
	bin, err := exec.LookPath("mosquitto")
	if err != nil {
		if _, e := os.Stat("/usr/sbin/mosquitto"); e == nil {
			return "/usr/sbin/mosquitto"
		}
		t.Skip("mosquitto not installed")
	}
	return bin
}
