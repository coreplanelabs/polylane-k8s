package platform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegisterWithRetrySucceedsAfterTransients(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			writeJSON(t, w, http.StatusOK, validResponseBody())
		}
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	var sleeps []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}

	resp, err := c.RegisterWithRetry(context.Background(), minimalRequest())
	if err != nil {
		t.Fatalf("RegisterWithRetry: %v", err)
	}
	if resp.AccountID != "acct_123" {
		t.Errorf("AccountID = %q, want acct_123", resp.AccountID)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server calls = %d, want 3", got)
	}
	if len(sleeps) != 2 {
		t.Fatalf("sleeps = %v, want 2 entries", sleeps)
	}
	// Attempt 0 had no Retry-After: full jitter in [0, backoffBase].
	if sleeps[0] < 0 || sleeps[0] > backoffBase {
		t.Errorf("sleeps[0] = %v, want within [0, %v]", sleeps[0], backoffBase)
	}
	// Attempt 1 sent Retry-After: 2 — honored exactly, no jitter.
	if sleeps[1] != 2*time.Second {
		t.Errorf("sleeps[1] = %v, want 2s", sleeps[1])
	}
}

func TestRegisterWithRetryTerminalReturnsImmediately(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(t, w, http.StatusUnauthorized, map[string]string{"error": "bad key"})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	c.sleep = func(context.Context, time.Duration) error {
		t.Error("sleep called; terminal errors must not retry")
		return nil
	}

	resp, err := c.RegisterWithRetry(context.Background(), minimalRequest())
	if resp != nil {
		t.Errorf("response = %+v, want nil", resp)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("errors.Is(err, ErrUnauthorized) = false (err: %v)", err)
	}
	if !IsTerminal(err) {
		t.Errorf("IsTerminal = false (err: %v)", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server calls = %d, want 1", got)
	}
}

func TestRegisterWithRetryHonorsRetryAfterDate(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", time.Now().Add(45*time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeJSON(t, w, http.StatusOK, validResponseBody())
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	var sleeps []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}

	if _, err := c.RegisterWithRetry(context.Background(), minimalRequest()); err != nil {
		t.Fatalf("RegisterWithRetry: %v", err)
	}
	if len(sleeps) != 1 {
		t.Fatalf("sleeps = %v, want 1 entry", sleeps)
	}
	// HTTP-date form has one-second granularity and time passes between
	// the server stamping it and the client parsing it.
	if sleeps[0] < 40*time.Second || sleeps[0] > 46*time.Second {
		t.Errorf("sleeps[0] = %v, want roughly 45s", sleeps[0])
	}
}

func TestRegisterWithRetryCapsRetryAfterAtBackoffCap(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(t, w, http.StatusOK, validResponseBody())
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	var sleeps []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}

	if _, err := c.RegisterWithRetry(context.Background(), minimalRequest()); err != nil {
		t.Fatalf("RegisterWithRetry: %v", err)
	}
	if len(sleeps) != 1 || sleeps[0] != backoffCap {
		t.Errorf("sleeps = %v, want exactly [%v]", sleeps, backoffCap)
	}
}

func TestRegisterWithRetryBackoffEnvelopeAndCancel(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	var sleeps []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		if len(sleeps) == 12 {
			return context.Canceled
		}
		return nil
	}

	resp, err := c.RegisterWithRetry(context.Background(), minimalRequest())
	if resp != nil {
		t.Errorf("response = %+v, want nil", resp)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	for i, d := range sleeps {
		ceiling := backoffCeiling(i)
		if d < 0 || d > ceiling {
			t.Errorf("sleeps[%d] = %v, want within [0, %v]", i, d, ceiling)
		}
	}
}

func TestRegisterWithRetryCancelDuringBackoff(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(Options{BaseURL: srv.URL, APIKey: "k", Logger: discardLogger()})
	// Exercise the real sleep against a context canceled mid-wait; the
	// hour-long timer must not actually elapse.
	c.sleep = func(inner context.Context, _ time.Duration) error {
		cancel()
		return sleepCtx(inner, time.Hour)
	}

	resp, err := c.RegisterWithRetry(ctx, minimalRequest())
	if resp != nil {
		t.Errorf("response = %+v, want nil", resp)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

func TestBackoffCeiling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{5, 32 * time.Second},
		{8, 256 * time.Second},
		{9, backoffCap},
		{10, backoffCap},
		{1000, backoffCap},
	}
	for _, tt := range tests {
		if got := backoffCeiling(tt.attempt); got != tt.want {
			t.Errorf("backoffCeiling(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestJitterWithinBounds(t *testing.T) {
	t.Parallel()
	const maxWait = 50 * time.Millisecond
	for range 200 {
		if d := jitter(maxWait); d < 0 || d > maxWait {
			t.Fatalf("jitter(%v) = %v, want within [0, %v]", maxWait, d, maxWait)
		}
	}
	if d := jitter(0); d != 0 {
		t.Errorf("jitter(0) = %v, want 0", d)
	}
	if d := jitter(-time.Second); d != 0 {
		t.Errorf("jitter(-1s) = %v, want 0", d)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"empty", "", 0},
		{"seconds", "7", 7 * time.Second},
		{"seconds_padded", " 7 ", 7 * time.Second},
		{"zero", "0", 0},
		{"negative", "-3", 0},
		{"http_date_future", now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		{"http_date_past", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"garbage", "soon", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseRetryAfter(tt.value, now); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestSleepCtx(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepCtx on canceled ctx = %v, want context.Canceled", err)
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleepCtx(1ms) = %v, want nil", err)
	}
}
