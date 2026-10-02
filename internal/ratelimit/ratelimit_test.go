// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
)

var t0 = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func newL(cfg Config) (*Limiter, *clock.Fake) {
	clk := clock.NewFake(t0)
	return New(cfg, clk, func() float64 { return 1 }), clk // rnd=1 => backoff is the full delay, deterministic
}

func acquire(t *testing.T, l *Limiter, host string) *Permit {
	t.Helper()
	p, err := l.Acquire(t.Context(), host, time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return p
}

func TestAcquire_BurstThenPaced(t *testing.T) {
	l, clk := newL(Config{Rate: 1, Burst: 2, MaxConcurrent: 10})
	acquire(t, l, "h").Release(Response{Status: 200})
	acquire(t, l, "h").Release(Response{Status: 200})

	_, err := l.Acquire(t.Context(), "h", 100*time.Millisecond)
	var we *WaitError
	if !errors.As(err, &we) || we.Reason != "pacing" {
		t.Fatalf("err=%v", err)
	}

	done := make(chan *Permit)
	go func() { p, _ := l.Acquire(t.Context(), "h", 0); done <- p }()
	if !clk.BlockUntil(1, time.Second) {
		t.Fatal("acquirer never waited")
	}
	clk.Advance(time.Second)
	if p := <-done; p == nil {
		t.Fatal("no permit after refill")
	}
}

func TestAcquire_ConcurrencyCap_BlocksUntilRelease(t *testing.T) {
	l, _ := newL(Config{Rate: 1000, Burst: 100, MaxConcurrent: 1})
	p1 := acquire(t, l, "h")
	got := make(chan struct{})
	go func() { p, _ := l.Acquire(t.Context(), "h", 0); p.Release(Response{Status: 200}); close(got) }()
	select {
	case <-got:
		t.Fatal("second acquire should block")
	case <-time.After(30 * time.Millisecond):
	}
	p1.Release(Response{Status: 200})
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("second acquire never proceeded")
	}
}

func TestAcquire_ContextCancel_Unblocks(t *testing.T) {
	l, _ := newL(Config{MaxConcurrent: 1})
	acquire(t, l, "h")
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error)
	go func() { _, err := l.Acquire(ctx, "h", 0); errc <- err }()
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestRelease_429RetryAfter_BlocksHostOnly(t *testing.T) {
	l, clk := newL(Config{Rate: 1000, Burst: 100})
	hdr := http.Header{"Retry-After": {"120"}}
	acquire(t, l, "a").Release(Response{Status: 429, Header: hdr})

	_, err := l.Acquire(t.Context(), "a", time.Second)
	var we *WaitError
	if !errors.As(err, &we) || !we.RetryAt.Equal(t0.Add(120*time.Second)) || we.Reason != "rate-limited" {
		t.Fatalf("err=%v", err)
	}
	acquire(t, l, "b").Release(Response{Status: 200}) // other hosts unaffected

	clk.Advance(120 * time.Second)
	acquire(t, l, "a").Release(Response{Status: 200})
}

func TestRelease_RetryAfterHTTPDate(t *testing.T) {
	l, _ := newL(Config{})
	hdr := http.Header{"Retry-After": {t0.Add(90 * time.Second).UTC().Format(http.TimeFormat)}}
	acquire(t, l, "a").Release(Response{Status: 429, Header: hdr})
	_, err := l.Acquire(t.Context(), "a", time.Second)
	var we *WaitError
	if !errors.As(err, &we) || we.RetryAt.Sub(t0) != 90*time.Second {
		t.Fatalf("err=%v", err)
	}
}

func TestRelease_GitHubSecondaryLimit403_Cools(t *testing.T) {
	l, _ := newL(Config{})
	acquire(t, l, "a").Release(Response{Status: 403, Header: http.Header{"Retry-After": {"60"}}})
	if _, err := l.Acquire(t.Context(), "a", time.Second); err == nil {
		t.Fatal("403 with Retry-After must cool down")
	}
}

func TestRelease_Plain403_DoesNotCool(t *testing.T) {
	l, _ := newL(Config{})
	acquire(t, l, "a").Release(Response{Status: 403})
	acquire(t, l, "a").Release(Response{Status: 200}) // a permission error is not a rate limit
}

func TestRelease_QuotaExhausted_WaitsForReset(t *testing.T) {
	l, clk := newL(Config{Rate: 1000, Burst: 100})
	reset := t0.Add(10 * time.Minute)
	hdr := http.Header{
		"X-Ratelimit-Limit": {"5000"}, "X-Ratelimit-Remaining": {"0"},
		"X-Ratelimit-Reset": {strconv.FormatInt(reset.Unix(), 10)},
	}
	acquire(t, l, "gh").Release(Response{Status: 200, Header: hdr})
	_, err := l.Acquire(t.Context(), "gh", time.Second)
	var we *WaitError
	if !errors.As(err, &we) || !we.RetryAt.Equal(reset) {
		t.Fatalf("err=%v", err)
	}
	if q := l.Budget("gh"); q.Limit != 5000 || q.Remaining != 0 {
		t.Fatalf("budget=%+v", q)
	}
	clk.Advance(10 * time.Minute)
	acquire(t, l, "gh").Release(Response{Status: 200})
}

func TestRelease_IETFDeltaReset(t *testing.T) {
	l, _ := newL(Config{})
	hdr := http.Header{"Ratelimit-Limit": {"100"}, "Ratelimit-Remaining": {"7"}, "Ratelimit-Reset": {"30"}}
	acquire(t, l, "gl").Release(Response{Status: 200, Header: hdr})
	if q := l.Budget("gl"); !q.Reset.Equal(t0.Add(30*time.Second)) || q.Remaining != 7 {
		t.Fatalf("budget=%+v", q)
	}
}

func TestHasReserve(t *testing.T) {
	l, clk := newL(Config{})
	if !l.HasReserve("x", 0.2) {
		t.Fatal("unknown quota counts as available")
	}
	hdr := http.Header{"X-Ratelimit-Limit": {"100"}, "X-Ratelimit-Remaining": {"15"}, "X-Ratelimit-Reset": {strconv.FormatInt(t0.Add(time.Hour).Unix(), 10)}}
	acquire(t, l, "x").Release(Response{Status: 200, Header: hdr})
	if l.HasReserve("x", 0.2) || !l.HasReserve("x", 0.1) {
		t.Fatal("reserve threshold wrong")
	}
	clk.Advance(2 * time.Hour)
	if !l.HasReserve("x", 0.2) {
		t.Fatal("after reset the quota is fresh")
	}
}

func TestRelease_ServerErrors_BackoffGrowsAndCircuitOpens(t *testing.T) {
	l, clk := newL(Config{Rate: 1000, Burst: 100, BackoffBase: 10 * time.Second, BackoffCap: time.Hour, BreakerThreshold: 3})
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second}
	for i, d := range want {
		acquire(t, l, "h").Release(Response{Status: 503})
		_, err := l.Acquire(t.Context(), "h", time.Second)
		var we *WaitError
		if !errors.As(err, &we) || we.RetryAt.Sub(clk.Now()) != d {
			t.Fatalf("failure %d: err=%v want wait %v", i+1, err, d)
		}
		clk.Advance(d)
	}
	st := l.Snapshot()[0]
	if !st.CircuitOpen || st.Failures != 3 {
		t.Fatalf("state=%+v", st)
	}
	acquire(t, l, "h").Release(Response{Status: 200}) // successful probe closes the circuit
	if st := l.Snapshot()[0]; st.CircuitOpen || st.Failures != 0 {
		t.Fatalf("state after success=%+v", st)
	}
}

func TestRelease_BackoffCapped(t *testing.T) {
	l, _ := newL(Config{BackoffBase: time.Minute, BackoffCap: 5 * time.Minute})
	if d := l.backoff(20); d != 5*time.Minute {
		t.Fatalf("d=%v", d)
	}
}

func TestRelease_TransportErrorCountsAsFailure(t *testing.T) {
	l, _ := newL(Config{})
	acquire(t, l, "h").Release(Response{Err: errors.New("dial tcp: refused")})
	if _, err := l.Acquire(t.Context(), "h", time.Second); err == nil {
		t.Fatal("expected backoff after transport error")
	}
}

func TestRelease_GitRateLimited_UsesProvidedRetryAfter(t *testing.T) {
	l, _ := newL(Config{})
	acquire(t, l, "h").Release(Response{RateLimited: true, RetryAfter: 45 * time.Second})
	_, err := l.Acquire(t.Context(), "h", time.Second)
	var we *WaitError
	if !errors.As(err, &we) || we.RetryAt.Sub(t0) != 45*time.Second {
		t.Fatalf("err=%v", err)
	}
}

func TestRelease_Twice_IsHarmless(t *testing.T) {
	l, _ := newL(Config{MaxConcurrent: 1, Rate: 1000, Burst: 100})
	p := acquire(t, l, "h")
	p.Release(Response{Status: 200})
	p.Release(Response{Status: 200})
	acquire(t, l, "h").Release(Response{Status: 200})
}

func TestSnapshot_ShowsCooldownAndInflight(t *testing.T) {
	l, _ := newL(Config{})
	p := acquire(t, l, "b")
	acquire(t, l, "a").Release(Response{Status: 429, Header: http.Header{"Retry-After": {"60"}}})
	s := l.Snapshot()
	if len(s) != 2 || s[0].Host != "a" || s[0].Cooldown.IsZero() || s[1].InFlight != 1 {
		t.Fatalf("snapshot=%+v", s)
	}
	p.Release(Response{Status: 200})
}

func TestRelease_HostileRetryAfterAndReset_AreCapped(t *testing.T) {
	l, _ := newL(Config{})
	acquire(t, l, "a").Release(Response{Status: 429, Header: http.Header{"Retry-After": {"99999999999"}}})
	_, err := l.Acquire(t.Context(), "a", time.Second)
	var we *WaitError
	if !errors.As(err, &we) || we.RetryAt.Sub(t0) > 24*time.Hour {
		t.Fatalf("retry-after not capped: %v", err)
	}
	acquire(t, l, "b").Release(Response{Status: 200, Header: http.Header{
		"X-Ratelimit-Limit": {"10"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"32503680000"}, // year 3000
	}})
	_, err = l.Acquire(t.Context(), "b", time.Second)
	if !errors.As(err, &we) || we.RetryAt.Sub(t0) > 24*time.Hour {
		t.Fatalf("reset not capped: %v", err)
	}
}

func FuzzRelease_HeaderParsing_NeverPanicsAndStaysBounded(f *testing.F) {
	for _, s := range []string{"120", "Wed, 21 Oct 2026 07:28:00 GMT", "-5", "abc", "99999999999999999999", "", "1e9", "0"} {
		f.Add(s, s, s, s)
	}
	f.Fuzz(func(t *testing.T, retry, limit, remaining, reset string) {
		l, clk := newL(Config{Rate: 1000, Burst: 100})
		p, err := l.Acquire(t.Context(), "h", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		p.Release(Response{Status: 429, Header: http.Header{
			"Retry-After": {retry}, "X-Ratelimit-Limit": {limit}, "X-Ratelimit-Remaining": {remaining}, "X-Ratelimit-Reset": {reset},
		}})
		if c := l.CooldownUntil("h"); c.Sub(clk.Now()) > 24*time.Hour+time.Second {
			t.Fatalf("cooldown %v exceeds the cap (retry=%q reset=%q)", c.Sub(clk.Now()), retry, reset)
		}
	})
}
