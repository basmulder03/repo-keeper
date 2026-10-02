// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
)

// realClockLimiter keeps tests fast: tiny base backoff on the real clock.
func newClient(t *testing.T, mod func(*Config)) *Client {
	t.Helper()
	clk := clock.Real{}
	cfg := Config{
		UserAgent: "repo-keeper/test (+https://example.invalid)",
		Limiter:   ratelimit.New(ratelimit.Config{Rate: 1000, Burst: 1000, BackoffBase: time.Millisecond, BackoffCap: 5 * time.Millisecond}, clk, nil),
		Clock:     clk,
	}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func get(t *testing.T, c *Client, url string) (*Response, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	return c.Do(t.Context(), req)
}

func TestNew_RequiresUAAndLimiter(t *testing.T) {
	if _, err := New(Config{Limiter: ratelimit.New(ratelimit.Config{}, clock.Real{}, nil)}); err == nil {
		t.Fatal("UA required")
	}
	if _, err := New(Config{UserAgent: "x"}); err == nil {
		t.Fatal("limiter required")
	}
}

func TestDo_SendsUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.UserAgent() }))
	defer srv.Close()
	if _, err := get(t, newClient(t, nil), srv.URL); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "repo-keeper/test") {
		t.Fatalf("UA=%q", got)
	}
}

func TestDo_RefusesPlainHTTPToRemoteHosts(t *testing.T) {
	for _, u := range []string{"http://example.com/x", "ftp://example.com/x", "http://10.0.0.5/x"} {
		if _, err := get(t, newClient(t, nil), u); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s: err=%v", u, err)
		}
	}
}

func TestDo_ErrorRedactsURLPassword(t *testing.T) {
	_, err := get(t, newClient(t, nil), "http://user:s3cret@127.0.0.1:1/x")
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err=%v", err)
	}
}

func TestDo_ETagCache_304ServedFromCache(t *testing.T) {
	var calls, conditional int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			atomic.AddInt32(&conditional, 1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()
	c := newClient(t, nil)
	first, _ := get(t, c, srv.URL)
	second, err := get(t, c, srv.URL)
	if err != nil || first.FromCache || !second.FromCache || string(second.Body) != "payload" || second.Status != 200 {
		t.Fatalf("first=%+v second=%+v err=%v", first, second, err)
	}
	if conditional != 1 || calls != 2 {
		t.Fatalf("calls=%d conditional=%d", calls, conditional)
	}
}

func TestDo_RetriesTransient5xxThenSucceeds(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	resp, err := get(t, newClient(t, nil), srv.URL)
	if err != nil || resp.Status != 200 || n != 3 {
		t.Fatalf("resp=%+v err=%v n=%d", resp, err, n)
	}
}

func TestDo_RateLimited_ReturnsWaitErrorInsteadOfBlocking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := newClient(t, func(cfg *Config) { cfg.MaxWait = time.Second })
	_, err := get(t, c, srv.URL)
	var we *ratelimit.WaitError
	if !errors.As(err, &we) {
		t.Fatalf("err=%v, want WaitError", err)
	}
	// and the next call is refused locally without touching the server
	_, err = get(t, c, srv.URL)
	if !errors.As(err, &we) {
		t.Fatalf("second err=%v", err)
	}
}

func TestDo_NonIdempotent_NotRetried(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("x"))
	resp, err := newClient(t, nil).Do(t.Context(), req)
	if err != nil || resp.Status != 503 || n != 1 {
		t.Fatalf("resp=%+v err=%v n=%d", resp, err, n)
	}
}

func TestDo_RetriesExhausted_ReturnsLastResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	resp, err := get(t, newClient(t, func(c *Config) { c.MaxRetries = 1 }), srv.URL)
	if err != nil || resp.Status != 500 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestCheckRedirect_RefusesDowngrade(t *testing.T) {
	https, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://a.example/x", http.NoBody)
	plain, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://a.example/y", http.NoBody)
	if err := checkRedirect(plain, []*http.Request{https}); err == nil {
		t.Fatal("downgrade must be refused")
	}
	if err := checkRedirect(https, []*http.Request{https}); err != nil {
		t.Fatal(err)
	}
	if err := checkRedirect(https, make([]*http.Request, 5)); err == nil {
		t.Fatal("redirect chain must be bounded")
	}
}

func TestDefaultTransport_NeverSkipsVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, err := get(t, newClient(t, nil), srv.URL) // self-signed, no RootCAs => must fail
	if err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	if _, err := get(t, newClient(t, func(c *Config) { c.RootCAs = pool }), srv.URL); err != nil {
		t.Fatalf("custom CA should work: %v", err)
	}
}

func TestDo_ETagCache_IsolatedPerCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"x"`)
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte("data-for-" + r.Header.Get("Authorization")))
	}))
	defer srv.Close()
	c := newClient(t, nil)
	do := func(auth string) *Response {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
		req.Header.Set("Authorization", auth)
		resp, err := c.Do(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	do("alice")
	bob := do("bob")
	if bob.FromCache || string(bob.Body) != "data-for-bob" {
		t.Fatalf("bob got %+v: cached data crossed credentials", bob)
	}
	if !do("alice").FromCache {
		t.Fatal("alice's own cache should still work")
	}
}
