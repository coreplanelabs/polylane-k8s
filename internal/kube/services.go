package kube

import (
	"context"
	"fmt"

	"github.com/coreplanelabs/polylane-k8s/internal/config"
)

type Service struct {
	Spec struct {
		Type      string            `json:"type"`
		ClusterIP string            `json:"clusterIP"`
		Selector  map[string]string `json:"selector"`
		Ports     []ServicePort     `json:"ports"`
	} `json:"spec"`
}

type ServicePort struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

func (c *Client) Service(ctx context.Context, namespace, name string) (Service, error) {
	var out Service
	if !config.ValidServiceLabel(namespace) || !config.ValidServiceLabel(name) {
		return out, fmt.Errorf("kube: invalid service name or namespace")
	}
	err := c.getJSON(ctx, "/api/v1/namespaces/"+namespace+"/services/"+name, &out)
	return out, err
}
