package customdomain

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"time"

	"deploy_platform/internal/netutil"
)

type Probe struct {
	Banned bool
	Tunnel bool
	OK     bool
	Status int
	Detail string
}

// ProbeEdge fetches the customer hostname and detects Cloudflare edge blocks.
// 1014 means a cross-account CNAME that the platform zone has not authorized.
// 1033 means the hostname is not published on the tunnel.
func ProbeEdge(ctx context.Context, hostname string) Probe {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "" {
		return Probe{Detail: "empty hostname"}
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 8*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+hostname+"/", nil)
	if err != nil {
		return Probe{Detail: "could not build probe"}
	}
	req.Header.Set("User-Agent", "RunexDomainProbe/1.0")
	client := netutil.SafeHTTPClient(8 * time.Second)
	// Keep TLS floor when the shared client transport is the default http.Transport.
	if tr, ok := client.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	res, err := client.Do(req)
	if err != nil {
		return Probe{Detail: "edge not reachable yet"}
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
	return classifyProbe(res.StatusCode, string(body))
}

// ProbeEdgeUntilReady retries Cloudflare edge probes while a custom hostname
// is still propagating (1014/1033). Stops early on a clean response.
func ProbeEdgeUntilReady(ctx context.Context, hostname string, attempts int, pause time.Duration) Probe {
	if attempts < 1 {
		attempts = 1
	}
	var last Probe
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return last
		}
		last = ProbeEdge(ctx, hostname)
		if last.OK && !last.Banned && !last.Tunnel {
			return last
		}
		if i == attempts-1 {
			break
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last
		case <-timer.C:
		}
	}
	return last
}

func classifyProbe(status int, body string) Probe {
	text := strings.ToLower(body)
	out := Probe{Status: status}
	switch {
	case strings.Contains(text, "error 1014") || strings.Contains(text, "error code: 1014") || strings.Contains(text, "cname cross-user banned"):
		out.Banned = true
		out.Detail = "Cloudflare Error 1014: this hostname is not authorized on the platform zone yet."
	case strings.Contains(text, "error 1033") || strings.Contains(text, "error code: 1033") || strings.Contains(text, "argo tunnel"):
		out.Tunnel = true
		out.Detail = "Cloudflare Error 1033: the hostname is not published on the platform tunnel yet."
	default:
		out.OK = status > 0 && status < 500
	}
	return out
}
