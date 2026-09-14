package config

import (
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
)

type Service struct {
	ID        string         `yaml:"id" json:"id"`
	Namespace string         `yaml:"namespace" json:"namespace"`
	Name      string         `yaml:"name" json:"name"`
	Port      int            `yaml:"port" json:"port"`
	Scheme    string         `yaml:"scheme" json:"scheme"`
	BasePath  string         `yaml:"base_path,omitempty" json:"base_path,omitempty"`
	Routes    []ServiceRoute `yaml:"routes" json:"routes"`
}

type ServiceRoute struct {
	Method     string `yaml:"method" json:"method"`
	Path       string `yaml:"path,omitempty" json:"path,omitempty"`
	PathPrefix string `yaml:"path_prefix,omitempty" json:"path_prefix,omitempty"`
}

var serviceLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidServiceLabel(value string) bool {
	return serviceLabel.MatchString(value)
}

func ValidateServices(services []Service) error {
	ids := make(map[string]bool, len(services))
	for _, s := range services {
		if !ValidServiceLabel(s.ID) || !ValidServiceLabel(s.Namespace) || !ValidServiceLabel(s.Name) {
			return fmt.Errorf("services: id, namespace and name must be DNS labels: %q", s.ID)
		}
		if ids[s.ID] {
			return fmt.Errorf("services: duplicate id %q", s.ID)
		}
		ids[s.ID] = true
		if s.Namespace == "default" && s.Name == "kubernetes" {
			return fmt.Errorf("services: Kubernetes API service is forbidden: %q", s.ID)
		}
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("services: port must be between 1 and 65535: %q", s.ID)
		}
		if s.Scheme != "http" && s.Scheme != "https" {
			return fmt.Errorf("services: scheme must be http or https: %q", s.ID)
		}
		if s.BasePath != "" && (!validServicePath(s.BasePath) || strings.HasSuffix(s.BasePath, "/")) {
			return fmt.Errorf("services: base_path must be canonical with no trailing slash: %q", s.ID)
		}
		if len(s.Routes) == 0 {
			return fmt.Errorf("services: at least one route is required: %q", s.ID)
		}
		for _, r := range s.Routes {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				return fmt.Errorf("services: unsupported route method for %q: %q", s.ID, r.Method)
			}
			if (r.Path == "") == (r.PathPrefix == "") {
				return fmt.Errorf("services: each route needs exactly one of path or path_prefix: %q", s.ID)
			}
			if r.Path != "" && !validServicePath(r.Path) {
				return fmt.Errorf("services: route path must be canonical: %q", s.ID)
			}
			if r.PathPrefix != "" && (!validServicePath(r.PathPrefix) || !strings.HasSuffix(r.PathPrefix, "/")) {
				return fmt.Errorf("services: path_prefix must be canonical and end in a slash: %q", s.ID)
			}
		}
	}
	return nil
}

func validServicePath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.ContainsAny(p, "%?#\\\r\n\t ") &&
		(p == "/" || path.Clean(p) == strings.TrimSuffix(p, "/"))
}
