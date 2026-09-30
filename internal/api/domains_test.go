package api

import (
	"testing"

	"deploy_platform/internal/config"
	"deploy_platform/internal/customdomain"
	"deploy_platform/internal/store"
)

func TestViewDomainHidesRawTokenField(t *testing.T) {
	s := &Server{cfg: config.Config{TunnelCNAME: "abc.cfargotunnel.com", EdgeDomain: "salesradar.live"}}
	view := s.viewDomain(t.Context(), store.CustomDomain{
		ID:       "dom_1",
		Hostname: "app.example.com",
		Status:   customdomain.StatusPending,
		Token:    "harbor-verify=secret",
	})
	if view.CNAMETarget == "" {
		t.Fatal("expected cname target")
	}
	if len(view.Records) < 1 {
		t.Fatalf("records %#v", view.Records)
	}
	if view.URL != "" {
		t.Fatal("pending domains must not get a public URL")
	}
	active := s.viewDomain(t.Context(), store.CustomDomain{
		ID:       "dom_1",
		Hostname: "app.example.com",
		Status:   customdomain.StatusActive,
		Token:    "harbor-verify=secret",
	})
	if active.URL != "https://app.example.com" {
		t.Fatalf("url %s", active.URL)
	}
}
