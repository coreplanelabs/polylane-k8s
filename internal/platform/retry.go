package platform

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// backoffBase is the jitter ceiling for the first retry.
	backoffBase = time.Second
	// backoffCap bounds every wait, including a server-supplied
	// Retry-After: registration must never park for longer than 5m, or a
	// recovered platform waits pointlessly on sleeping agents.
	backoffCap = 5 * time.Minute
)

// RegisterWithRetry calls Register until it succeeds, fails terminally,
// or ctx is canceled. Transient failures wait a full-jitter backoff — a
// uniform draw from [0, min(5m, 1s·2^attempt)] — unless the platform sent
// Retry-After, which is honored verbatim (capped at 5m). Terminal errors
// and malformed success responses return immediately.
func (c *Client) RegisterWithRetry(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.Register(ctx, req)
		if err == nil {
			return resp, nil
		}
		var transient *TransientError
		if !errors.As(err, &transient) {
			return nil, err
		}
		wait := transient.RetryAfter
		if wait <= 0 {
			wait = jitter(backoffCeiling(attempt))
		}
		if wait > backoffCap {
			wait = backoffCap
		}
		c.logger.Warn("registration attempt failed; backing off",
			"attempt", attempt+1, "wait", wait, "error", err)
		if sleepErr := c.sleep(ctx, wait); sleepErr != nil {
			return nil, fmt.Errorf("platform: registration canceled while backing off: %w (last error: %v)", sleepErr, err)
		}
	}
}

// backoffCeiling is the exponential envelope: backoffBase doubled per
// attempt, saturating at backoffCap.
func backoffCeiling(attempt int) time.Duration {
	ceiling := backoffBase
	for range attempt {
		ceiling *= 2
		if ceiling >= backoffCap {
			return backoffCap
		}
	}
	return ceiling
}

// jitter draws a uniformly random duration in [0, maxWait]. Full jitter
// keeps a fleet of agents from synchronizing retries after a platform
// outage. crypto/rand needs no seeding and costs nothing at retry
// frequency.
func jitter(maxWait time.Duration) time.Duration {
	if maxWait <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxWait)+1))
	if err != nil {
		// A failing system randomness source is a broken host; wait the
		// full ceiling rather than hammering the platform.
		return maxWait
	}
	return time.Duration(n.Int64())
}

// parseRetryAfter interprets a Retry-After value in either RFC 9110 form:
// delay-seconds or an HTTP-date. Absent, malformed, or already-elapsed
// values yield 0, sending the caller to jittered backoff instead.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	if d := when.Sub(now); d > 0 {
		return d
	}
	return 0
}

// sleepCtx is the production sleep: a context-aware timer. Tests inject a
// recording replacement via Client.sleep.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
