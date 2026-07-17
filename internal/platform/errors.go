package platform

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrUnauthorized matches (via errors.Is) any TerminalError carrying HTTP
// 401 or 403, so the agent can exit with a distinct code and operators
// see "bad API key" instead of a crash loop.
var ErrUnauthorized = errors.New("platform: unauthorized")

// TerminalError is a rejection that retrying cannot repair: 400 (bad
// request), 401/403 (credentials), 426 (agent too old), and any other
// status outside the transient set.
type TerminalError struct {
	StatusCode int
	Message    string
}

func (e *TerminalError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("platform: registration rejected (http %d)", e.StatusCode)
	}
	return fmt.Sprintf("platform: registration rejected (http %d): %s", e.StatusCode, e.Message)
}

// Is reports ErrUnauthorized for credential rejections so callers write
// errors.Is(err, ErrUnauthorized) instead of inspecting status codes.
func (e *TerminalError) Is(target error) bool {
	if target != ErrUnauthorized {
		return false
	}
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// IsTerminal reports whether err carries a TerminalError anywhere in its
// chain.
func IsTerminal(err error) bool {
	var terminal *TerminalError
	return errors.As(err, &terminal)
}

// TransientError is a failure that may resolve on its own: HTTP 429, any
// 5xx, or a transport-level error (StatusCode 0, cause in Err).
// RetryAfter carries the server's Retry-After hint when it sent one; zero
// means the caller chooses the backoff.
type TransientError struct {
	StatusCode int
	RetryAfter time.Duration
	// Err is the underlying transport error for network failures; nil
	// when the failure was an HTTP status.
	Err error
}

func (e *TransientError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("platform: request failed: %v", e.Err)
	}
	return fmt.Sprintf("platform: transient failure (http %d)", e.StatusCode)
}

func (e *TransientError) Unwrap() error { return e.Err }
