// Package cloudflare publishes verified custom hostnames onto a remotely
// managed Cloudflare Tunnel. PUT replaces the whole ingress list, so every
// write GETs first and never drops existing platform rules.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"deploy_platform/internal/config"
)

type Tunnel struct {
	token         string
	accountID     string
	tunnelID      string
	zoneID        string
	zoneName      string
	fallback      string
	fallbackReady bool
	origin        string
	http          *http.Client
	mu            sync.Mutex
}

func NewTunnel(cfg config.Config) *Tunnel {
	token := strings.TrimSpace(cfg.CloudflareAPIToken)
	if token == "" {
		return nil
	}
	return &Tunnel{
		token:     token,
		accountID: strings.TrimSpace(cfg.CloudflareAccountID),
		tunnelID:  strings.TrimSpace(cfg.CloudflareTunnelID),
		zoneID:    strings.TrimSpace(cfg.CloudflareZoneID),
		zoneName:  strings.ToLower(strings.TrimSpace(cfg.EdgeDomain)),
		fallback:  cfg.DomainCNAME(),
		origin:    cfg.CustomDomainOrigin(),
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

func (t *Tunnel) Enabled() bool {
	return t != nil && t.token != "" && (t.saasEnabled() || t.ingressEnabled())
}

func (t *Tunnel) ingressEnabled() bool {
	return t != nil && t.token != "" && t.accountID != "" && t.tunnelID != ""
}

func (t *Tunnel) EnsureHostname(ctx context.Context, hostname string) error {
	if t == nil || t.token == "" {
		return fmt.Errorf("cloudflare API token is not configured")
	}
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "" {
		return fmt.Errorf("empty hostname")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureSaaS(ctx, hostname); err != nil {
		return err
	}
	if t.ingressEnabled() {
		if err := t.ensureIngress(ctx, hostname); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tunnel) RemoveHostname(ctx context.Context, hostname string) error {
	if t == nil || t.token == "" {
		return nil
	}
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.deleteCustomHostname(ctx, hostname)
	if t.ingressEnabled() {
		_ = t.removeIngress(ctx, hostname)
	}
	return nil
}

func (t *Tunnel) ensureIngress(ctx context.Context, hostname string) error {
	cfg, err := t.getConfig(ctx)
	if err != nil {
		return err
	}
	ingress, _ := ingressOf(cfg)
	next := upsertIngress(ingress, hostname, t.origin)
	if ingressEqual(ingress, next) {
		return nil
	}
	cfg["ingress"] = next
	return t.putConfig(ctx, cfg)
}

func (t *Tunnel) removeIngress(ctx context.Context, hostname string) error {
	cfg, err := t.getConfig(ctx)
	if err != nil {
		return err
	}
	ingress, _ := ingressOf(cfg)
	next := removeIngress(ingress, hostname)
	if ingressEqual(ingress, next) {
		return nil
	}
	cfg["ingress"] = next
	return t.putConfig(ctx, cfg)
}

func (t *Tunnel) getConfig(ctx context.Context) (map[string]any, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/configurations", t.accountID, t.tunnelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	t.auth(req)
	res, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("cloudflare tunnel get: %s", statusText(res.StatusCode))
	}
	var parsed struct {
		Success bool `json:"success"`
		Result  struct {
			Config map[string]any `json:"config"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if parsed.Result.Config == nil {
		return map[string]any{}, nil
	}
	return parsed.Result.Config, nil
}

func (t *Tunnel) putConfig(ctx context.Context, cfg map[string]any) error {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/configurations", t.accountID, t.tunnelID)
	payload, err := json.Marshal(map[string]any{"config": cfg})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	t.auth(req)
	req.Header.Set("Content-Type", "application/json")
	res, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode >= 300 {
		return fmt.Errorf("cloudflare tunnel put: %s", statusText(res.StatusCode))
	}
	return nil
}

func (t *Tunnel) auth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+t.token)
}

func ingressOf(cfg map[string]any) ([]map[string]any, bool) {
	raw, ok := cfg["ingress"]
	if !ok || raw == nil {
		return nil, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var out []map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, false
	}
	return out, true
}

func upsertIngress(ingress []map[string]any, hostname, service string) []map[string]any {
	var rules, catch []map[string]any
	replaced := false
	for _, rule := range ingress {
		host, _ := rule["hostname"].(string)
		if strings.TrimSpace(host) == "" {
			catch = append(catch, rule)
			continue
		}
		if strings.EqualFold(host, hostname) {
			rule["hostname"] = hostname
			rule["service"] = service
			rules = append(rules, rule)
			replaced = true
			continue
		}
		rules = append(rules, rule)
	}
	if !replaced {
		rules = append(rules, map[string]any{
			"hostname": hostname,
			"service":  service,
		})
	}
	if len(catch) == 0 {
		catch = []map[string]any{{"service": "http_status:404"}}
	}
	return append(rules, catch...)
}

func removeIngress(ingress []map[string]any, hostname string) []map[string]any {
	var rules, catch []map[string]any
	for _, rule := range ingress {
		host, _ := rule["hostname"].(string)
		if strings.TrimSpace(host) == "" {
			catch = append(catch, rule)
			continue
		}
		if strings.EqualFold(host, hostname) {
			continue
		}
		rules = append(rules, rule)
	}
	if len(catch) == 0 {
		catch = []map[string]any{{"service": "http_status:404"}}
	}
	return append(rules, catch...)
}

func ingressEqual(a, b []map[string]any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func statusText(code int) string {
	return http.StatusText(code)
}
