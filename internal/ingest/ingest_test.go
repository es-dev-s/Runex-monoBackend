package ingest

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestParseGitHubOwnerRepoAndURL(t *testing.T) {
	cases := []struct {
		in    string
		owner string
		name  string
	}{
		{"acme/app", "acme", "app"},
		{"https://github.com/acme/app", "acme", "app"},
		{"https://github.com/acme/app.git", "acme", "app"},
		{"github.com/acme/app", "acme", "app"},
	}
	for _, tc := range cases {
		ref, err := ParseGitHub(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if ref.Owner != tc.owner || ref.Name != tc.name {
			t.Fatalf("%s: %+v", tc.in, ref)
		}
	}
}

func TestAuthedCloneURLEmbedsInstallationToken(t *testing.T) {
	ref := GitRef{Owner: "acme", Name: "app", HTTPS: "https://github.com/acme/app.git"}
	got := authedCloneURL(ref, "ghs_test")
	if got != "https://x-access-token:ghs_test@github.com/acme/app.git" {
		t.Fatalf("got %q", got)
	}
	if authedCloneURL(ref, "") != ref.HTTPS {
		t.Fatal("empty token should stay public")
	}
}

func TestCloneOptsRejectBadRefs(t *testing.T) {
	if err := (CloneOpts{Branch: "../etc"}).validate(); err == nil {
		t.Fatal("expected invalid branch")
	}
	if err := (CloneOpts{SHA: "not-a-sha"}).validate(); err == nil {
		t.Fatal("expected invalid sha")
	}
	if err := (CloneOpts{Branch: "main", SHA: "abc1234"}).validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseGitHubRejects(t *testing.T) {
	for _, in := range []string{"", "not github", "https://gitlab.com/a/b", "../etc/passwd", "a/b/c"} {
		if _, err := ParseGitHub(in); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}

func TestExtractZipRejectsSlip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	_, err := zw.Create("../escape.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	err = ExtractZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), dir, 10, 1<<20)
	if err == nil {
		t.Fatal("expected zip slip error")
	}
}

func TestWriteHTMLAlwaysIndex(t *testing.T) {
	dir := t.TempDir()
	if err := WriteHTML(dir, "page.html", "<h1>ok</h1>"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "<h1>ok</h1>" {
		t.Fatalf("got %q", raw)
	}
}
