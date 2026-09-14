package config

import (
	"strings"
	"testing"
)

func TestServiceConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*Service)
		valid  bool
	}{
		{"http", func(*Service) {}, true},
		{"https", func(s *Service) { s.Scheme = "https" }, true},
		{"base path", func(s *Service) { s.BasePath = "/grafana" }, true},
		{"exact query", func(s *Service) { s.Routes = []ServiceRoute{{Method: "POST", Path: "/api/ds/query"}} }, true},
		{"invalid id", func(s *Service) { s.ID = "../grafana" }, false},
		{"invalid namespace", func(s *Service) { s.Namespace = "monitoring/../default" }, false},
		{"invalid name", func(s *Service) { s.Name = "metadata.example.com" }, false},
		{"long name", func(s *Service) { s.Name = strings.Repeat("a", 64) }, false},
		{"kube api", func(s *Service) { s.Namespace, s.Name = "default", "kubernetes" }, false},
		{"zero port", func(s *Service) { s.Port = 0 }, false},
		{"large port", func(s *Service) { s.Port = 65536 }, false},
		{"missing scheme", func(s *Service) { s.Scheme = "" }, false},
		{"tcp", func(s *Service) { s.Scheme = "tcp" }, false},
		{"trailing base slash", func(s *Service) { s.BasePath = "/grafana/" }, false},
		{"base traversal", func(s *Service) { s.BasePath = "/grafana/../api" }, false},
		{"base query", func(s *Service) { s.BasePath = "/grafana?target=foo" }, false},
		{"no routes", func(s *Service) { s.Routes = nil }, false},
		{"connect", func(s *Service) { s.Routes[0].Method = "CONNECT" }, false},
		{"wildcard method", func(s *Service) { s.Routes[0].Method = "*" }, false},
		{"both paths", func(s *Service) { s.Routes[0].Path = "/api" }, false},
		{"neither path", func(s *Service) { s.Routes[0].PathPrefix = "" }, false},
		{"ambiguous prefix", func(s *Service) { s.Routes[0].PathPrefix = "/api" }, false},
		{"encoded prefix", func(s *Service) { s.Routes[0].PathPrefix = "/api/%2e%2e/" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validBase()
			service := Service{ID: "grafana", Namespace: "monitoring", Name: "grafana", Port: 3000, Scheme: "http",
				Routes: []ServiceRoute{{Method: "GET", PathPrefix: "/api/"}}}
			tt.change(&service)
			cfg.Services = []Service{service}
			if err := cfg.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Validate() = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestLoadServices(t *testing.T) {
	t.Parallel()
	file := writeTemp(t, `state_secret:
  name: polylane-state
services:
  - id: grafana
    namespace: monitoring
    name: grafana
    port: 80
    scheme: http
    routes:
      - method: POST
        path: /api/ds/query
`)
	cfg, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Services) != 1 || cfg.Services[0].Port != 80 || cfg.Services[0].Routes[0].Path != "/api/ds/query" {
		t.Fatalf("services = %+v", cfg.Services)
	}
	cfg.Services = append(cfg.Services, cfg.Services[0])
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate service: %v", err)
	}
}
