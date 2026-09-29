package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type PushEvent struct {
	Ref            string
	After          string
	Deleted        bool
	InstallationID int64
	FullName       string
	DefaultBranch  string
	Branch         string
}

type InstallationChange struct {
	Action         string
	InstallationID int64
	Account        string
	Type           string
}

func VerifyWebhook(secret, header string, body []byte) bool {
	if strings.TrimSpace(secret) == "" || strings.TrimSpace(header) == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(header)))
}

func ParsePush(body []byte) (PushEvent, error) {
	var payload struct {
		Ref          string `json:"ref"`
		After        string `json:"after"`
		Deleted      bool   `json:"deleted"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Repository struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return PushEvent{}, err
	}
	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
	if branch == payload.Ref {
		branch = ""
	}
	fullName := strings.TrimSpace(payload.Repository.FullName)
	if fullName == "" {
		return PushEvent{}, fmt.Errorf("missing repository")
	}
	return PushEvent{
		Ref:            payload.Ref,
		After:          strings.TrimSpace(payload.After),
		Deleted:        payload.Deleted,
		InstallationID: payload.Installation.ID,
		FullName:       fullName,
		DefaultBranch:  payload.Repository.DefaultBranch,
		Branch:         branch,
	}, nil
}

func ParseInstallation(body []byte) (InstallationChange, error) {
	var payload struct {
		Action       string `json:"action"`
		Installation struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return InstallationChange{}, err
	}
	return InstallationChange{
		Action:         payload.Action,
		InstallationID: payload.Installation.ID,
		Account:        payload.Installation.Account.Login,
		Type:           payload.Installation.Account.Type,
	}, nil
}
