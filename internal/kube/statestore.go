package kube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Secret data keys. Exact spelling is load-bearing: cloudflared mounts the
// Secret as a directory and reads the tunnelToken file via TUNNEL_TOKEN_FILE.
const (
	keyTunnelToken    = "tunnelToken"
	keyShimSecret     = "shimSecret"
	keyAccountID      = "accountId"
	keyTunnelID       = "tunnelId"
	keyTunnelHostname = "tunnelHostname"
	keyRegisteredAt   = "registeredAt"
)

// State is the agent's registration state as persisted in the state Secret.
type State struct {
	TunnelToken    string
	ShimSecret     string
	AccountID      string
	TunnelID       string
	TunnelHostname string
	// RegisteredAt is stored as RFC3339 in the secret.
	RegisteredAt time.Time
}

// Complete reports whether every credential field is present; an
// incomplete state forces re-registration rather than a half-working boot.
func (s State) Complete() bool {
	return s.TunnelToken != "" &&
		s.ShimSecret != "" &&
		s.AccountID != "" &&
		s.TunnelID != "" &&
		s.TunnelHostname != ""
}

// StateStore persists State into a pre-created Secret. It only ever GETs
// and PATCHes — creating the Secret is the chart's job, and RBAC pins the
// agent to that one resourceName, so the agent can never mint or read
// other Secrets.
type StateStore struct {
	client     *Client
	namespace  string
	secretName string
}

// NewStateStore returns a StateStore for the named Secret in namespace.
func NewStateStore(c *Client, namespace, secretName string) *StateStore {
	return &StateStore{client: c, namespace: namespace, secretName: secretName}
}

// Load reads the state Secret. A Secret with no data is a normal first
// boot and yields (nil, nil); a missing Secret is a broken install (the
// chart must pre-create it) and yields an error.
func (s *StateStore) Load(ctx context.Context) (*State, error) {
	path := s.secretPath()
	req, err := s.client.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kube: GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("kube: state secret %s/%s not found; the chart must pre-create it", s.namespace, s.secretName)
	}
	if !is2xx(resp.StatusCode) {
		return nil, statusError(http.MethodGet, path, resp)
	}

	// map[string][]byte base64-decodes each data value during JSON
	// decoding, exactly how the API serializes Secret data.
	var secret struct {
		Data map[string][]byte `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseBytes)).Decode(&secret); err != nil {
		return nil, fmt.Errorf("kube: GET %s: decoding secret: %w", path, err)
	}
	if len(secret.Data) == 0 {
		return nil, nil
	}

	st := &State{
		TunnelToken:    string(secret.Data[keyTunnelToken]),
		ShimSecret:     string(secret.Data[keyShimSecret]),
		AccountID:      string(secret.Data[keyAccountID]),
		TunnelID:       string(secret.Data[keyTunnelID]),
		TunnelHostname: string(secret.Data[keyTunnelHostname]),
	}
	if raw := secret.Data[keyRegisteredAt]; len(raw) > 0 {
		t, err := time.Parse(time.RFC3339, string(raw))
		if err != nil {
			return nil, fmt.Errorf("kube: state secret %s/%s: parsing %s: %w", s.namespace, s.secretName, keyRegisteredAt, err)
		}
		st.RegisteredAt = t
	}
	return st, nil
}

// Save writes st into the Secret with a strategic-merge PATCH: update-only
// semantics (a PATCH on a missing Secret 404s), and unrelated keys other
// controllers may have added survive.
func (s *StateStore) Save(ctx context.Context, st State) error {
	patch := struct {
		Data map[string]string `json:"data"`
	}{Data: map[string]string{
		keyTunnelToken:    base64.StdEncoding.EncodeToString([]byte(st.TunnelToken)),
		keyShimSecret:     base64.StdEncoding.EncodeToString([]byte(st.ShimSecret)),
		keyAccountID:      base64.StdEncoding.EncodeToString([]byte(st.AccountID)),
		keyTunnelID:       base64.StdEncoding.EncodeToString([]byte(st.TunnelID)),
		keyTunnelHostname: base64.StdEncoding.EncodeToString([]byte(st.TunnelHostname)),
		keyRegisteredAt:   base64.StdEncoding.EncodeToString([]byte(st.RegisteredAt.UTC().Format(time.RFC3339))),
	}}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("kube: encoding state patch: %w", err)
	}

	path := s.secretPath()
	req, err := s.client.newRequest(ctx, http.MethodPatch, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")
	resp, err := s.client.hc.Do(req)
	if err != nil {
		return fmt.Errorf("kube: PATCH %s: %w", path, err)
	}
	defer resp.Body.Close()
	if !is2xx(resp.StatusCode) {
		return statusError(http.MethodPatch, path, resp)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxAPIResponseBytes))
	return nil
}

func (s *StateStore) secretPath() string {
	return "/api/v1/namespaces/" + url.PathEscape(s.namespace) + "/secrets/" + url.PathEscape(s.secretName)
}
