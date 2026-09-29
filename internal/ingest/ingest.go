package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

var (
	ErrInvalidURL  = errors.New("invalid GitHub repository")
	ErrInvalidZip  = errors.New("invalid zip archive")
	ErrZipSlip     = errors.New("zip entry escapes destination")
	ErrZipTooLarge = errors.New("zip archive exceeds size limit")
	ErrCloneFailed = errors.New("git clone failed")
)

type GitRef struct {
	Owner string
	Name  string
	HTTPS string
}

func ParseGitHub(raw string) (GitRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return GitRef{}, ErrInvalidURL
	}
	if !strings.Contains(raw, "://") && !strings.Contains(raw, "github.com") {
		return parseOwnerRepo(raw)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil {
		return GitRef{}, ErrInvalidURL
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return GitRef{}, ErrInvalidURL
	}
	parts := strings.FieldsFunc(parsed.Path, func(r rune) bool { return r == '/' })
	if len(parts) < 2 {
		return GitRef{}, ErrInvalidURL
	}
	return parseOwnerRepo(parts[0] + "/" + parts[1])
}

func parseOwnerRepo(value string) (GitRef, error) {
	value = strings.Trim(strings.TrimSuffix(value, ".git"), "/")
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !safeName(parts[0]) || !safeName(parts[1]) {
		return GitRef{}, ErrInvalidURL
	}
	return GitRef{
		Owner: parts[0],
		Name:  parts[1],
		HTTPS: "https://github.com/" + parts[0] + "/" + parts[1] + ".git",
	}, nil
}

func safeName(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

type CloneOpts struct {
	Token  string
	Branch string
	SHA    string
}

func CloneGitHub(ctx context.Context, dest string, ref GitRef, opts CloneOpts) error {
	if err := opts.validate(); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	cloneURL := authedCloneURL(ref, opts.Token)
	if opts.SHA != "" {
		return cloneSHA(ctx, dest, cloneURL, opts)
	}
	args := []string{"clone", "--depth", "1", "--single-branch", "--no-tags"}
	if opts.Branch != "" {
		args = append(args, "--branch", opts.Branch)
	}
	args = append(args, "--", cloneURL, dest)
	return runGit(ctx, opts.Token, args...)
}

func authedCloneURL(ref GitRef, token string) string {
	if token == "" {
		return ref.HTTPS
	}
	u := url.URL{
		Scheme: "https",
		User:   url.UserPassword("x-access-token", token),
		Host:   "github.com",
		Path:   "/" + ref.Owner + "/" + ref.Name + ".git",
	}
	return u.String()
}

func cloneSHA(ctx context.Context, dest, cloneURL string, opts CloneOpts) error {
	if err := runGit(ctx, opts.Token, "init", dest); err != nil {
		return err
	}
	if err := runGit(ctx, opts.Token, "-C", dest, "remote", "add", "origin", cloneURL); err != nil {
		return err
	}
	if err := runGit(ctx, opts.Token, "-C", dest, "fetch", "--depth", "1", "origin", opts.SHA); err != nil {
		return err
	}
	return runGit(ctx, opts.Token, "-C", dest, "checkout", "--force", "FETCH_HEAD")
}

func (o CloneOpts) validate() error {
	if o.Branch != "" && !safeGitRef(o.Branch) {
		return fmt.Errorf("%w: invalid branch", ErrInvalidURL)
	}
	if o.SHA != "" && !safeSHA(o.SHA) {
		return fmt.Errorf("%w: invalid commit", ErrInvalidURL)
	}
	return nil
}

func safeGitRef(value string) bool {
	if value == "" || len(value) > 255 || strings.Contains(value, "..") {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' || r == '/' {
			continue
		}
		return false
	}
	return true
}

func safeSHA(value string) bool {
	if len(value) < 7 || len(value) > 40 {
		return false
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

func runGit(ctx context.Context, token string, args ...string) error {
	full := append([]string{"-c", "core.askPass=", "-c", "credential.helper="}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "***")
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%w: %s", ErrCloneFailed, msg)
	}
	return nil
}

func ExtractZip(src io.ReaderAt, size int64, dest string, maxFiles int, maxUncompressed int64) error {
	reader, err := zip.NewReader(src, size)
	if err != nil {
		return ErrInvalidZip
	}
	if len(reader.File) > maxFiles {
		return ErrZipTooLarge
	}
	var uncompressed int64
	for _, file := range reader.File {
		uncompressed += int64(file.UncompressedSize64)
		if uncompressed > maxUncompressed {
			return ErrZipTooLarge
		}
		if file.Mode()&os.ModeSymlink != 0 {
			return ErrInvalidZip
		}
	}
	for _, file := range reader.File {
		if err := extractFile(file, dest); err != nil {
			return err
		}
	}
	return stripSingleRoot(dest)
}

func extractFile(file *zip.File, dest string) error {
	name := strings.ReplaceAll(file.Name, "\\", "/")
	if name == "" || strings.HasPrefix(name, "/") {
		return ErrZipSlip
	}
	cleaned := filepath.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return ErrZipSlip
	}
	target := filepath.Join(dest, cleaned)
	rel, err := filepath.Rel(dest, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ErrZipSlip
	}
	if file.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	in, err := file.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func stripSingleRoot(dest string) error {
	entries, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}
	root := filepath.Join(dest, entries[0].Name())
	tmp := dest + ".unwrapped"
	if err := os.Rename(root, tmp); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func WriteHTML(dest, name, body string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	_ = name
	return os.WriteFile(filepath.Join(dest, "index.html"), []byte(body), 0o644)
}
