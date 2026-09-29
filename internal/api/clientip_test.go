package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIPPrefersCFConnectingIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.5:12345"
	r.Header.Set("X-Forwarded-For", "198.51.100.200")
	r.Header.Set("X-Real-IP", "198.51.100.201")
	r.Header.Set("CF-Connecting-IP", "203.0.113.50")
	if got := clientIP(r); got != "203.0.113.50" {
		t.Fatalf("got %q want CF-Connecting-IP", got)
	}
}

func TestClientIPIgnoresSpoofedForwardedHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.5:12345"
	r.Header.Set("X-Forwarded-For", "198.51.100.200, 10.0.0.1")
	r.Header.Set("X-Real-IP", "198.51.100.201")
	if got := clientIP(r); got != "10.0.0.5" {
		t.Fatalf("spoofed XFF must not win: got %q", got)
	}
}
