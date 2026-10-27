package client_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
)

func writeTestKeyPair(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "media-fixer-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err = os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestNewLoki_MTLSKeepsTheClonedTransport(t *testing.T) {
	t.Parallel()
	certPath, keyPath := writeTestKeyPair(t)

	c, err := client.NewLoki("https://loki.example", certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}

	tr, ok := c.HTTPClientForTest().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.HTTPClientForTest().Transport)
	}
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	if tr.Proxy == nil {
		t.Error("Proxy is nil, want the default transport's proxy setting")
	}
	if tr.TLSHandshakeTimeout != def.TLSHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %v, want %v", tr.TLSHandshakeTimeout, def.TLSHandshakeTimeout)
	}
	if tr.IdleConnTimeout != def.IdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, def.IdleConnTimeout)
	}
	if tr.ExpectContinueTimeout != def.ExpectContinueTimeout {
		t.Errorf("ExpectContinueTimeout = %v, want %v", tr.ExpectContinueTimeout, def.ExpectContinueTimeout)
	}
	if tr.DialContext == nil {
		t.Error("DialContext is nil, want the default dialer with its timeouts")
	}
	if tr.TLSClientConfig == nil || len(tr.TLSClientConfig.Certificates) != 1 || tr.TLSClientConfig.RootCAs == nil {
		t.Errorf("TLSClientConfig = %+v, want the client cert and a root pool", tr.TLSClientConfig)
	}
}
