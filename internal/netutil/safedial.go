package netutil

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// IsBlockedIP reports addresses an attacker could use for SSRF
// (loopback, RFC1918, link-local, CGNAT, metadata, etc.).
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 100.64.0.0/10 CGNAT
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		// 169.254.0.0/16 link-local / cloud metadata
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
		// 0.0.0.0/8
		if ip4[0] == 0 {
			return true
		}
	}
	return false
}

// SafeHTTPClient returns an HTTP client that refuses connections to blocked IPs
// (after DNS resolution) and enforces a timeout.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 0,
		Control: func(network, address string, c syscall.RawConn) error {
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:               nil, // never honor env HTTP_PROXY for user-triggered fetches
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 5 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			// Reject literal blocked IPs before dial.
			if ip := net.ParseIP(host); ip != nil {
				if IsBlockedIP(ip) {
					return nil, fmt.Errorf("refusing connection to blocked address")
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var last error
			for _, ipa := range ips {
				if IsBlockedIP(ipa.IP) {
					last = fmt.Errorf("refusing connection to blocked address")
					continue
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			if last == nil {
				last = fmt.Errorf("no safe addresses for host")
			}
			return nil, last
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return http.ErrUseLastResponse
			}
			// Re-check redirect target host is not an IP we block (DialContext also checks).
			return nil
		},
	}
}
