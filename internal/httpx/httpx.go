// SPDX-License-Identifier: Apache-2.0

// Package httpx is the only outbound HTTP path: identifiable UA, TLS policy, rate limiting, retries, ETag cache.
package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/ratelimit"
)

const maxBody = 32 << 20

// Config configures a Client.
type Config struct {
	// UserAgent identifies repo-keeper to providers (CMP-2); required.
	UserAgent string
	Limiter   *ratelimit.Limiter
	Clock     clock.Clock
	// RootCAs optionally adds a private CA (self-hosted instances). TLS verification is never disabled.
	RootCAs *x509.CertPool
	// Transport overrides the default transport (tests).
	Transport http.RoundTripper
	Timeout   time.Duration
	// MaxRetries for idempotent requests on 429/5xx (default 3).
	MaxRetries int
	// MaxWait bounds how long one call may wait on the limiter before returning *ratelimit.WaitError; 0 = ctx only.
	MaxWait time.Duration
}

// Client performs rate-limited requests.
type Client struct {
	cfg   Config
	hc    *http.Client
	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	etag   string
	body   []byte
	header http.Header
}

// Response is a fully read response.
type Response struct {
	Status    int
	Header    http.Header
	Body      []byte
	FromCache bool // a 304 was answered from the ETag cache
}

// New validates cfg and builds a Client.
func New(cfg Config) (*Client, error) {
	if cfg.UserAgent == "" {
		return nil, errors.New("httpx: UserAgent is required")
	}
	if cfg.Limiter == nil {
		return nil, errors.New("httpx: Limiter is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	rt := cfg.Transport
	if rt == nil {
		rt = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: cfg.RootCAs},
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		}
	}
	c := &Client{cfg: cfg, cache: map[string]cached{}}
	c.hc = &http.Client{Transport: rt, Timeout: cfg.Timeout, CheckRedirect: checkRedirect}
	return c, nil
}

// checkRedirect refuses https->http downgrades and long chains; stdlib already strips credentials cross-host.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("httpx: too many redirects")
	}
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("httpx: refusing https->http redirect")
	}
	return nil
}

// allowed enforces https, except for loopback hosts (local test servers, never remote).
func allowed(u *url.URL) error {
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("httpx: refusing non-https URL %q", u.Redacted())
}

// Do sends req through the limiter. Authorization, if any, is set by the caller on req.
func (c *Client) Do(ctx context.Context, req *http.Request) (*Response, error) {
	if err := allowed(req.URL); err != nil {
		return nil, err
	}
	idempotent := req.Method == http.MethodGet || req.Method == http.MethodHead
	for attempt := 0; ; attempt++ {
		resp, retry, err := c.once(ctx, req)
		if err == nil && !retry {
			return resp, nil
		}
		if err != nil && !retry { // permanent failure (certificate problem, local refusal): retrying cannot help
			return nil, err
		}
		var we *ratelimit.WaitError
		if errors.As(err, &we) || ctx.Err() != nil {
			return nil, err
		}
		if !idempotent || attempt >= c.cfg.MaxRetries {
			if err != nil {
				return nil, err
			}
			return resp, nil // exhausted retries: hand the last (error) response to the caller
		}
	}
}

// once performs a single attempt; retry is true for 429/5xx/transport errors. The limiter enforces the wait on re-entry.
func (c *Client) once(ctx context.Context, req *http.Request) (resp *Response, retry bool, err error) {
	host := req.URL.Host
	permit, err := c.cfg.Limiter.Acquire(ctx, host, c.cfg.MaxWait)
	if err != nil {
		return nil, false, err
	}
	r := req.Clone(ctx)
	r.Header.Set("User-Agent", c.cfg.UserAgent)
	key := ""
	if r.Method == http.MethodGet {
		key = cacheKey(r)
		c.mu.Lock()
		if e, ok := c.cache[key]; ok && r.Header.Get("If-None-Match") == "" {
			r.Header.Set("If-None-Match", e.etag)
		}
		c.mu.Unlock()
	}

	// #nosec G704 -- URL validated by allowed(): https, or loopback http for tests
	hr, err := c.hc.Do(r) //nolint:gosec // see #nosec above
	if err != nil {
		if isCertError(err) {
			permit.Release(ratelimit.Response{Status: http.StatusOK}) // our trust configuration, not the host's health
			return nil, false, fmt.Errorf("httpx: %s %s: %w (add the issuing CA with ca_file if this is a private instance)", r.Method, r.URL.Redacted(), err)
		}
		permit.Release(ratelimit.Response{Err: err})
		return nil, true, fmt.Errorf("httpx: %s %s: %w", r.Method, r.URL.Redacted(), err)
	}
	defer func() { _ = hr.Body.Close() }()
	body, rerr := io.ReadAll(io.LimitReader(hr.Body, maxBody+1))
	if rerr == nil && len(body) > maxBody {
		rerr = errors.New("response too large")
	}
	if rerr != nil {
		permit.Release(ratelimit.Response{Status: hr.StatusCode, Header: hr.Header, Err: rerr})
		return nil, true, fmt.Errorf("httpx: reading %s: %w", r.URL.Redacted(), rerr)
	}
	permit.Release(ratelimit.Response{Status: hr.StatusCode, Header: hr.Header})

	out := &Response{Status: hr.StatusCode, Header: hr.Header, Body: body}
	switch {
	case hr.StatusCode == http.StatusNotModified && key != "":
		c.mu.Lock()
		e, ok := c.cache[key]
		c.mu.Unlock()
		if ok {
			return &Response{Status: http.StatusOK, Header: e.header, Body: bytes.Clone(e.body), FromCache: true}, false, nil
		}
	case hr.StatusCode == http.StatusOK && key != "" && hr.Header.Get("ETag") != "":
		c.mu.Lock()
		if len(c.cache) > 512 { // crude bound; discovery paging is the only heavy user
			c.cache = map[string]cached{}
		}
		c.cache[key] = cached{etag: hr.Header.Get("ETag"), body: bytes.Clone(body), header: hr.Header.Clone()}
		c.mu.Unlock()
	}
	retryable := hr.StatusCode == http.StatusTooManyRequests || hr.StatusCode >= 500 ||
		(hr.StatusCode == http.StatusForbidden && hr.Header.Get("Retry-After") != "")
	return out, retryable, nil
}

// cacheKey scopes cached bodies to the credential, so two accounts on one host never see each other's data.
func cacheKey(r *http.Request) string {
	sum := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	return hex.EncodeToString(sum[:8]) + " " + r.URL.String()
}

// LoadCAFile returns the system roots plus the certificates in a PEM file (private CA of a self-hosted instance).
// Verification stays on; this only widens the set of trusted issuers for that account.
func LoadCAFile(path string) (*x509.CertPool, error) {
	// #nosec G304 -- user-configured CA bundle, opened read-only
	f, err := os.Open(path) //nolint:gosec // see #nosec above
	if err != nil {
		return nil, fmt.Errorf("httpx: %w", err)
	}
	defer func() { _ = f.Close() }()
	pem, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(pem) > 1<<20 {
		return nil, fmt.Errorf("httpx: cannot read CA file %s", path)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("httpx: %s contains no PEM certificates", path)
	}
	return pool, nil
}

// isCertError reports TLS verification failures (unknown issuer, wrong host, expired): never transient, never the host's fault.
func isCertError(err error) bool {
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var ci x509.CertificateInvalidError
	return errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &hn) || errors.As(err, &ci)
}
