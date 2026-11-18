package direct

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// device makes a self-signed certificate, as each Yonder has one, and
// gives it with its fingerprint.
func device(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "a device"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:])
}

var errWrongCert = errors.New("the device's certificate does not match its fingerprint")

// client makes an HTTP client as localsend's Sender does: it dials through
// the Peer, and takes only the certificate with this fingerprint.
func client(p *Peer, fingerprint string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return p.Dial(ctx) },
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // replaced by the fingerprint check below
			VerifyConnection: func(cs tls.ConnectionState) error {
				sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
				if hex.EncodeToString(sum[:]) != fingerprint {
					return errWrongCert
				}
				return nil
			},
		},
	}}
}

// What the later steps need: TLS with a pinned certificate runs inside a
// conn, and an http.Server and an http.Client run on that, unchanged. And
// the condition of the security review: with the channel open and the
// inner certificate wrong, nothing is transferred.
func TestTLSAndHTTPOverAConn(t *testing.T) {
	a, b := connect(t)
	cert, fingerprint := device(t)

	var requests atomic.Int64
	srv := &http.Server{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			h := sha256.New()
			n, _ := io.Copy(h, r.Body)
			w.Header().Set("X-Bytes", big.NewInt(n).String())
			w.Write(h.Sum(nil))
		}),
	}
	served := make(chan error, 1)
	go func() { served <- srv.ServeTLS(b, "", "") }()

	post := func(c *http.Client, body []byte) (*http.Response, []byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), patience)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", "https://device/upload", bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		return resp, got, err
	}

	t.Run("the pinned certificate", func(t *testing.T) {
		c := client(a, fingerprint)
		defer c.CloseIdleConnections()
		body := noise(5, 4<<20)
		for range 2 { // the second request uses the first's connection again
			resp, got, err := post(c, body)
			if err != nil {
				t.Fatal(err)
			}
			want := sha256.Sum256(body)
			if resp.StatusCode != 200 || !bytes.Equal(got, want[:]) || resp.Header.Get("X-Bytes") != "4194304" {
				t.Fatalf("status %d, %s bytes arrived, hash equal: %v", resp.StatusCode, resp.Header.Get("X-Bytes"), bytes.Equal(got, want[:]))
			}
		}
	})

	t.Run("the inner certificate is wrong", func(t *testing.T) {
		before := requests.Load()
		_, other := device(t)
		c := client(a, other)
		defer c.CloseIdleConnections()
		_, _, err := post(c, noise(6, 1<<20))
		if !errors.Is(err, errWrongCert) {
			t.Fatalf("the request gave %v", err)
		}
		if requests.Load() != before {
			t.Error("the request reached the handler")
		}
		// The channel layer is as it was: the transfer stopped at TLS.
		if a.Err() != nil || b.Err() != nil {
			t.Errorf("a Peer ended: %v, %v", a.Err(), b.Err())
		}
		good := client(a, fingerprint)
		defer good.CloseIdleConnections()
		if _, _, err := post(good, []byte("still open")); err != nil {
			t.Errorf("the Peer takes no request after that: %v", err)
		}
	})

	// Closing the Peer ends the server, as closing a listener does.
	b.Close()
	if err := within(t, "the server to end", func() error { return <-served }); !errors.Is(err, net.ErrClosed) {
		t.Errorf("ServeTLS = %v", err)
	}
}
