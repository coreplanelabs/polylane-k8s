package buildinfo

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	t.Parallel()
	s := String()
	for _, part := range []string{Version, "commit " + Commit, "built " + BuildTime, "go"} {
		if !strings.Contains(s, part) {
			t.Errorf("String() = %q, missing %q", s, part)
		}
	}
}

func TestUserAgent(t *testing.T) {
	t.Parallel()
	if got, want := UserAgent(), "polylane-k8s/"+Version; got != want {
		t.Errorf("UserAgent() = %q, want %q", got, want)
	}
}
