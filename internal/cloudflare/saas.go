package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type apiEnvelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (t *Tunnel) saasEnabled() bool {
	return t != nil && t.token != "" && (t.zoneID != "" || t.zoneName != "")
}

func (t *Tunnel) ensureSaaS(ctx context.Context, hostname string) error {
	if !t.saasEnabled() {
		return fmt.Errorf("cloudflare for saas is not configured")
	}
	if err := t.resolveZone(ctx); err != nil {
		return err
	}
	if err := t.ensureFallbackOrigin(ctx); err != nil {
		return err
	}
	return t.ensureCustomHostname(ctx, hostname)
}

func (t *Tunnel) resolveZone(ctx context.Context) error {
	if t.zoneID != "" {
		return nil
	}
	name := strings.ToLower(strings.TrimSpace(t.zoneName))
	if name == "" {
		return fmt.Errorf("cloudflare zone is not configured")
	}
	endpoint := "https://api.cloudflare.com/client/v4/zones?name=" + url.QueryEscape(name) + "&per_page=1"
	body, err := t.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	var zones []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &zones); err != nil {
		return fmt.Errorf("cloudflare zone lookup: %w", err)
	}
	if len(zones) == 0 || strings.TrimSpace(zones[0].ID) == "" {
		return fmt.Errorf("cloudflare zone %s was not found for this API token", name)
	}
	t.zoneID = zones[0].ID
	return nil
}

func (t *Tunnel) ensureFallbackOrigin(ctx context.Context) error {
	if t.fallback == "" || t.zoneID == "" {
		return nil
	}
	if t.fallbackReady {
		return nil
	}
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/custom_hostnames/fallback_origin", t.zoneID)
	body, err := t.do(ctx, http.MethodGet, endpoint, nil)
	if err == nil {
		var current struct {
			Origin string `json:"origin"`
			Status string `json:"status"`
		}
		if json.Unmarshal(body, &current) == nil && strings.EqualFold(strings.TrimSpace(current.Origin), t.fallback) {
			t.fallbackReady = strings.EqualFold(current.Status, "active") || current.Status != ""
			if t.fallbackReady {
				return nil
			}
		}
	}
	payload, err := json.Marshal(map[string]any{"origin": t.fallback})
	if err != nil {
		return err
	}
	if _, err := t.do(ctx, http.MethodPut, endpoint, payload); err != nil {
		return fmt.Errorf("cloudflare fallback origin: %w", err)
	}
	t.fallbackReady = true
	return nil
}

func (t *Tunnel) ensureCustomHostname(ctx context.Context, hostname string) error {
	if t.zoneID == "" {
		return fmt.Errorf("cloudflare zone is not configured")
	}
	existing, origin, err := t.getCustomHostname(ctx, hostname)
	if err != nil {
		return err
	}
	if existing != "" {
		return t.patchCustomOrigin(ctx, existing, origin)
	}
	payload, err := json.Marshal(t.hostnamePayload(hostname))
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/custom_hostnames", t.zoneID)
	_, err = t.do(ctx, http.MethodPost, endpoint, payload)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") || strings.Contains(strings.ToLower(err.Error()), "already exists") {
			id, current, lookErr := t.getCustomHostname(ctx, hostname)
			if lookErr != nil {
				return lookErr
			}
			return t.patchCustomOrigin(ctx, id, current)
		}
		return fmt.Errorf("cloudflare custom hostname: %w", err)
	}
	return nil
}

func (t *Tunnel) hostnamePayload(hostname string) map[string]any {
	// Do not set custom_origin_server / custom_origin_sni — those are
	// Cloudflare SSL for SaaS Enterprise settings and break custom domains
	// on standard plans. Fallback origin is configured zone-wide instead.
	return map[string]any{
		"hostname": hostname,
		"ssl": map[string]any{
			"method": "http",
			"type":   "dv",
			"settings": map[string]any{
				"min_tls_version": "1.2",
			},
		},
	}
}

func (t *Tunnel) patchCustomOrigin(ctx context.Context, id, current string) error {
	// Enterprise custom-origin fields intentionally omitted (see hostnamePayload).
	_ = id
	_ = current
	return nil
}

func (t *Tunnel) getCustomHostname(ctx context.Context, hostname string) (string, string, error) {
	endpoint := fmt.Sprintf(
		"https://api.cloudflare.com/client/v4/zones/%s/custom_hostnames?hostname=%s",
		t.zoneID, url.QueryEscape(hostname),
	)
	body, err := t.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	var rows []struct {
		ID                 string `json:"id"`
		CustomOriginServer string `json:"custom_origin_server"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return "", "", err
	}
	if len(rows) == 0 {
		return "", "", nil
	}
	return rows[0].ID, rows[0].CustomOriginServer, nil
}

func (t *Tunnel) deleteCustomHostname(ctx context.Context, hostname string) error {
	if t.zoneID == "" {
		if err := t.resolveZone(ctx); err != nil {
			return nil
		}
	}
	id, _, err := t.getCustomHostname(ctx, hostname)
	if err != nil || id == "" {
		return nil
	}
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/custom_hostnames/%s", t.zoneID, id)
	_, _ = t.do(ctx, http.MethodDelete, endpoint, nil)
	return nil
}

func (t *Tunnel) do(ctx context.Context, method, endpoint string, payload []byte) (json.RawMessage, error) {
	var reader io.Reader
	if len(payload) > 0 {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	t.auth(req)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var parsed apiEnvelope
	if err := json.Unmarshal(raw, &parsed); err != nil {
		if res.StatusCode >= 300 {
			return nil, fmt.Errorf("%s", statusText(res.StatusCode))
		}
		return nil, fmt.Errorf("cloudflare: invalid response")
	}
	if res.StatusCode >= 300 || !parsed.Success {
		return nil, fmt.Errorf("%s", apiErrorText(res.StatusCode, parsed.Errors))
	}
	if len(parsed.Result) == 0 {
		return json.RawMessage("null"), nil
	}
	return parsed.Result, nil
}

func apiErrorText(status int, errors []apiError) string {
	var parts []string
	for _, item := range errors {
		msg := strings.TrimSpace(item.Message)
		if msg == "" {
			continue
		}
		parts = append(parts, msg)
	}
	joined := strings.Join(parts, "; ")
	lower := strings.ToLower(joined)
	switch {
	case status == http.StatusPaymentRequired || strings.Contains(lower, "saas") && strings.Contains(lower, "not entitl"):
		return "Cloudflare for SaaS is not enabled on the platform zone"
	case status == http.StatusForbidden && joined == "":
		return "Cloudflare API token cannot manage custom hostnames on this zone"
	case joined != "":
		return joined
	default:
		return statusText(status)
	}
}
