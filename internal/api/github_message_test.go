package api

import (
	"errors"
	"testing"
)

func TestGitHubReposMessage(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("github token: {\"message\":\"A JSON web token could not be decoded\"}"), "GitHub App credentials are invalid. Recreate the private key for this app."},
		{errors.New("github token: {\"message\":\"Not Found\"} // 404"), "GitHub denied repository access. Connect GitHub again and grant the repositories you want to deploy."},
		{errors.New("github repos: {\"message\":\"Bad credentials\"} 401"), "GitHub denied repository access. Connect GitHub again and grant the repositories you want to deploy."},
		{errors.New("github repos: API rate limit exceeded"), "GitHub rate-limited repository listing. Try again in a minute."},
		{errors.New("connection refused"), "Could not list GitHub repositories. Reconnect GitHub and grant repository access."},
	}
	for _, item := range cases {
		if got := githubReposMessage(item.err); got != item.want {
			t.Fatalf("githubReposMessage(%v)\n got %q\nwant %q", item.err, got, item.want)
		}
	}
}

func TestGitHubNeedsReconnect(t *testing.T) {
	if !githubNeedsReconnect(errors.New("github token: Not Found 404")) {
		t.Fatal("404 should ask the user to reconnect")
	}
	if githubNeedsReconnect(errors.New("connection refused")) {
		t.Fatal("a transport failure is not an install problem")
	}
}
