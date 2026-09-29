package cloudflare

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"deploy_platform/internal/config"
)

func TestEnsureCustomHostnameCreatesThenResolvesZone(t *testing.T) {
	created := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/client/v4/zones":
			io.WriteString(w, `{"success":true,"errors":[],"result":[{"id":"zone_1"}]}`)
		case strings.HasSuffix(r.URL.Path, "/fallback_origin") && r.Method == http.MethodGet:
			io.WriteString(w, `{"success":true,"errors":[],"result":{"origin":"cname.salesradar.live","status":"active"}}`)
		case strings.Contains(r.URL.Path, "/custom_hostnames") && r.Method == http.MethodGet:
			io.WriteString(w, `{"success":true,"errors":[],"result":[]}`)
		case strings.Contains(r.URL.Path, "/custom_hostnames") && r.Method == http.MethodPost:
			created++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["hostname"] != "designflow.aivex.art" {
				t.Fatalf("hostname %v", body["hostname"])
			}
			if _, ok := body["custom_origin_sni"]; ok {
				t.Fatal("custom origin SNI is enterprise-only and must not be sent")
			}
			if _, ok := body["custom_origin_server"]; ok {
				t.Fatal("custom origin server is enterprise-only and must not be sent")
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"success":true,"errors":[],"result":{"id":"ch_1","hostname":"designflow.aivex.art"}}`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	tun := &Tunnel{
		token:    "tok",
		zoneName: "salesradar.live",
		fallback: "cname.salesradar.live",
		http:     &http.Client{Transport: rewriteHost{base: srv.URL}},
	}
	if err := tun.ensureSaaS(context.Background(), "designflow.aivex.art"); err != nil {
		t.Fatal(err)
	}
	if created != 1 || tun.zoneID != "zone_1" {
		t.Fatalf("created=%d zone=%s", created, tun.zoneID)
	}
}

func TestNewTunnelEnabledWithTokenOnly(t *testing.T) {
	tun := NewTunnel(config.Config{
		CloudflareAPIToken: "tok",
		EdgeDomain:         "salesradar.live",
		CNAMETarget:        "cname.salesradar.live",
	})
	if tun == nil || !tun.Enabled() {
		t.Fatal("token + zone name should enable SaaS publishing")
	}
}

type rewriteHost struct {
	base string
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	target, err := url.Parse(r.base)
	if err != nil {
		return nil, err
	}
	clone.URL.Scheme = target.Scheme
	clone.URL.Host = target.Host
	clone.Host = target.Host
	return http.DefaultTransport.RoundTrip(clone)
}
