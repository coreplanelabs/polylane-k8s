package kube

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestService(t *testing.T) {
	t.Parallel()
	tc := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/monitoring/services/grafana" {
			t.Errorf("unexpected kube request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("service lookup must authenticate with the projected token")
		}
		fmt.Fprint(w, `{"spec":{"type":"ClusterIP","clusterIP":"10.96.0.20","selector":{"app":"grafana"},"ports":[{"port":80,"targetPort":3000,"protocol":"TCP"}]}}`)
	}))
	service, err := tc.c.Service(context.Background(), "monitoring", "grafana")
	if err != nil {
		t.Fatal(err)
	}
	if service.Spec.ClusterIP != "10.96.0.20" || service.Spec.Type != "ClusterIP" || service.Spec.Ports[0].Port != 80 || service.Spec.Selector["app"] != "grafana" {
		t.Fatalf("service = %+v", service)
	}
	if _, err := tc.c.Service(context.Background(), "../default", "kubernetes"); err == nil {
		t.Fatal("service lookup accepted path traversal")
	}
}
