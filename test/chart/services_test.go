package chart_test

import (
	"testing"

	"github.com/coreplanelabs/polylane-k8s/internal/config"
	"gopkg.in/yaml.v3"
)

func TestServiceConfig(t *testing.T) {
	t.Parallel()
	rendered := render(t, "apiKey.existingSecret=k",
		"config.services[0].id=grafana", "config.services[0].namespace=monitoring", "config.services[0].name=grafana",
		"config.services[0].port=80", "config.services[0].scheme=http",
		"config.services[0].routes[0].method=POST", "config.services[0].routes[0].path=/api/ds/query")
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(doc(t, rendered, "ConfigMap")), &cm); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Services) != 1 || cfg.Services[0].ID != "grafana" || cfg.Services[0].Port != 80 || cfg.Services[0].Routes[0].Path != "/api/ds/query" {
		t.Fatalf("services = %+v", cfg.Services)
	}
}
