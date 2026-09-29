package netutil

import (
	"net"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "10.0.0.1", "192.168.1.1", "172.16.5.5", "169.254.169.254", "100.64.1.1", "0.0.0.0", "224.0.0.1"}
	for _, s := range blocked {
		if !IsBlockedIP(net.ParseIP(s)) {
			t.Fatalf("expected blocked: %s", s)
		}
	}
	if IsBlockedIP(net.ParseIP("1.1.1.1")) {
		t.Fatal("1.1.1.1 should be allowed")
	}
	if IsBlockedIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("8.8.8.8 should be allowed")
	}
}
