package serviceproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreplanelabs/polylane-k8s/internal/kube"
)

func serviceCertificate(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"grafana.monitoring.svc"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, cert
}

func TestHTTPTransport(t *testing.T) {
	// An environment proxy must never receive service traffic or credentials.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, mode := range []string{"http", "https", "untrusted https"} {
		t.Run(mode, func(t *testing.T) {
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "grafana.monitoring.svc:80" || r.Header.Get("Authorization") != "Bearer app-token" || r.Header.Get("X-Polylane-Shim-Key") != "" {
					t.Errorf("upstream Host = %s, headers = %v", r.Host, r.Header)
				}
				if mode == "https" && (r.TLS == nil || r.TLS.ServerName != "grafana.monitoring.svc") {
					t.Error("wrong TLS server name")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"org":"test"}`)
			}))
			t.Cleanup(upstream.Close)
			binding := grafanaBinding()
			var root *x509.Certificate
			if mode == "http" {
				upstream.Start()
			} else {
				binding.Scheme = "https"
				cert, parsed := serviceCertificate(t)
				root = parsed
				upstream.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
				upstream.StartTLS()
			}
			h := newProxy(t, binding, func(context.Context, string, string) (kube.Service, error) { return grafanaService(), nil }, nil)
			transport, ok := h.targets[binding.ID].client.Transport.(*http.Transport)
			if !ok {
				t.Fatal("expected HTTP transport")
			}
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if addr != "10.96.0.20:80" {
					t.Errorf("dialed %s, expected resolved ClusterIP", addr)
				}
				return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
			}
			if mode == "https" {
				transport.TLSClientConfig.RootCAs = x509.NewCertPool()
				transport.TLSClientConfig.RootCAs.AddCert(root)
			}
			r := request("GET", "/services/grafana/api/org", "")
			r.Header.Set("Authorization", "Bearer app-token")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if mode == "untrusted https" {
				if w.Code != 502 {
					t.Fatalf("untrusted certificate accepted: %d", w.Code)
				}
			} else if w.Code != 200 || w.Body.String() != `{"org":"test"}` {
				t.Fatalf("response = %d %s", w.Code, w.Body)
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestResponseLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		size        int64
		declared    int64
		status      int
		wantReadErr bool
	}{
		{"known oversized", maxResponseBytes + 1, maxResponseBytes + 1, 502, false},
		{"stream exactly at cap", maxResponseBytes, -1, 200, false},
		{"oversized stream", maxResponseBytes + 1, -1, 200, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newProxy(t, grafanaBinding(), func(context.Context, string, string) (kube.Service, error) { return grafanaService(), nil }, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: tt.declared,
					Body: io.NopCloser(io.LimitReader(zeroReader{}, tt.size))}, nil
			}))
			server := httptest.NewServer(h)
			t.Cleanup(server.Close)
			res, err := server.Client().Get(server.URL + "/services/grafana/api/search")
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			n, err := io.Copy(io.Discard, res.Body)
			if res.StatusCode != tt.status || (err != nil) != tt.wantReadErr {
				t.Fatalf("response = %d, bytes = %d, read error = %v", res.StatusCode, n, err)
			}
			if tt.status == 200 && !tt.wantReadErr && n != tt.size {
				t.Fatalf("received %d bytes, want %d", n, tt.size)
			}
		})
	}
}

func TestEscapedPathsAndHopHeaders(t *testing.T) {
	t.Parallel()
	h := newProxy(t, grafanaBinding(), func(context.Context, string, string) (kube.Service, error) { return grafanaService(), nil }, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.EscapedPath() != "/api/dashboards/hello%20world" || r.Header.Get("Authorization") != "" {
			t.Errorf("request = %s, headers = %v", r.URL, r.Header)
		}
		res := response(200, "ok")
		res.Header.Set("Connection", "Retry-After")
		res.Header.Set("Retry-After", "5")
		return res, nil
	}))
	r := request("GET", "/services/grafana/api/dashboards/hello%20world", "")
	r.Header.Set("Connection", "Authorization")
	r.Header.Set("Authorization", "must-not-forward")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Header().Get("Retry-After"), "5") {
		t.Fatalf("response = %d %v", w.Code, w.Header())
	}
}
