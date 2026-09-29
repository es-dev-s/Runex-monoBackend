package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

func EncodeIntentState(secret, intentID string) string {
	id := strings.TrimSpace(intentID)
	if id == "" {
		return EncodeState(secret, "/deployments")
	}
	return EncodeState(secret, "/i/"+id)
}

func DecodeIntentState(secret, state string) (string, error) {
	path, err := DecodeState(secret, state)
	if err != nil {
		return "", err
	}
	id, ok := strings.CutPrefix(path, "/i/")
	if !ok || !validIntentID(id) {
		return "", fmt.Errorf("invalid intent")
	}
	return id, nil
}

func validIntentID(id string) bool {
	if len(id) < 32 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func EncodeUserState(secret, userID, returnPath string) string {
	path := SafeReturnPath(returnPath)
	raw := strings.TrimSpace(userID) + "\n" + path
	payload := base64.RawURLEncoding.EncodeToString([]byte(raw))
	return payload + "." + sign(secret, payload)
}

func DecodeUserState(secret, state string) (string, string, error) {
	path, err := DecodeState(secret, state)
	if err != nil {
		return "", "/deployments", err
	}
	payload, _, ok := strings.Cut(strings.TrimSpace(state), ".")
	if !ok {
		return "", path, fmt.Errorf("invalid state")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", path, err
	}
	userID, rest, ok := strings.Cut(string(raw), "\n")
	if !ok {
		return "", SafeReturnPath(string(raw)), nil
	}
	return strings.TrimSpace(userID), SafeReturnPath(rest), nil
}

func EncodeState(secret, returnPath string) string {
	path := SafeReturnPath(returnPath)
	payload := base64.RawURLEncoding.EncodeToString([]byte(path))
	return payload + "." + sign(secret, payload)
}

func DecodeState(secret, state string) (string, error) {
	state = strings.TrimSpace(state)
	if state == "" {
		return "/deployments", fmt.Errorf("missing state")
	}
	payload, sig, ok := strings.Cut(state, ".")
	if !ok || payload == "" || sig == "" {
		return "/deployments", fmt.Errorf("invalid state")
	}
	if !hmac.Equal([]byte(sig), []byte(sign(secret, payload))) {
		return "/deployments", fmt.Errorf("invalid state signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "/deployments", err
	}
	return SafeReturnPath(string(raw)), nil
}

func SafeReturnPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/deployments"
	}
	if strings.Contains(raw, "://") || strings.ContainsAny(raw, "\\ \t\n\r") || len(raw) > 200 {
		return "/deployments"
	}
	return raw
}

func sign(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}
