package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/oklog/ulid"

	"github.com/dstotijn/hetty/pkg/reqid"
)

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate CA key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Hetty Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create CA certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("failed to parse CA certificate: %v", err)
	}

	return cert, key
}

func newTestProxy(t *testing.T) *Proxy {
	t.Helper()

	cert, key := testCA(t)

	p, err := NewProxy(Config{CACert: cert, CAKey: key})
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}

	return p
}

// proxiedRequest executes a plain HTTP request through the proxy to an
// httptest backend and returns the recorded response and backend request.
func proxiedRequest(t *testing.T, p *Proxy, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	return rec
}

func TestServeHTTPGeneratesRequestIDAndEchoesHeader(t *testing.T) {
	var seenReqID ulid.ULID

	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		id, ok := reqid.FromContext(r.Context())
		if ok {
			seenReqID = id
		}
	}))
	defer backend.Close()

	p := newTestProxy(t)

	backendURL, _ := url.Parse(backend.URL)
	req := httptest.NewRequest(http.MethodGet, backendURL.String(), nil)
	// Rewrite into proxy request form: absolute URL.
	req.RequestURI = backendURL.String()

	rec := proxiedRequest(t, p, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %v", rec.Code)
	}

	headerID := rec.Header().Get(reqid.HeaderName)
	if headerID == "" {
		t.Fatal("expected X-Request-ID response header")
	}

	if seenReqID.Compare(ulid.ULID{}) == 0 {
		t.Fatal("expected proxied upstream request to carry a request ID")
	}

	if headerID != seenReqID.String() {
		t.Fatalf("header ID %q != context ID %q", headerID, seenReqID.String())
	}
}

func TestServeHTTPPrefersContextID(t *testing.T) {
	var seenReqID ulid.ULID

	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenReqID, _ = reqid.FromContext(r.Context())
	}))
	defer backend.Close()

	p := newTestProxy(t)
	existing := reqid.New()

	backendURL, _ := url.Parse(backend.URL)
	req := httptest.NewRequest(http.MethodGet, backendURL.String(), nil)
	req.RequestURI = backendURL.String()
	*req = *req.WithContext(reqid.ContextWithID(req.Context(), existing))

	rec := proxiedRequest(t, p, req)

	if seenReqID.Compare(existing) != 0 {
		t.Fatalf("expected proxy to reuse context ID %v, got %v", existing, seenReqID)
	}

	if got := rec.Header().Get(reqid.HeaderName); got != existing.String() {
		t.Fatalf("expected header to echo context ID, got %q", got)
	}
}
