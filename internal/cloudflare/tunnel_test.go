package cloudflare

import "testing"

func TestUpsertIngressKeepsCatchAllLast(t *testing.T) {
	existing := []map[string]any{
		{"hostname": "deploy.salesradar.live", "service": "http://127.0.0.1:80"},
		{"hostname": "*.salesradar.live", "service": "http://127.0.0.1:80"},
		{"service": "http_status:404"},
	}
	next := upsertIngress(existing, "app.example.com", "http://127.0.0.1:18080")
	if len(next) != 4 {
		t.Fatalf("len %d", len(next))
	}
	if next[len(next)-1]["hostname"] != nil && next[len(next)-1]["hostname"] != "" {
		t.Fatalf("catch-all must stay last: %#v", next[len(next)-1])
	}
	if next[2]["hostname"] != "app.example.com" || next[2]["service"] != "http://127.0.0.1:18080" {
		t.Fatalf("custom hostname %#v", next[2])
	}
	again := upsertIngress(next, "app.example.com", "http://127.0.0.1:18080")
	if !ingressEqual(next, again) {
		t.Fatal("idempotent upsert")
	}
	removed := removeIngress(next, "app.example.com")
	if !ingressEqual(existing, removed) {
		t.Fatalf("remove should restore platform rules: %#v", removed)
	}
	trimmed := removeIngress(existing, "deploy.salesradar.live")
	if len(trimmed) != 2 {
		t.Fatalf("removed platform hostname leftover %#v", trimmed)
	}
	if host, _ := trimmed[len(trimmed)-1]["hostname"].(string); host != "" {
		t.Fatal("removing a hostname must keep the catch-all last")
	}
}
