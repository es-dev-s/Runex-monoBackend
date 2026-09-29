package detect

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"deploy_platform/internal/pack"
)

type Client struct {
	base string
	http *http.Client
}

func New(base string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 90 * time.Second},
	}
}

type Language struct {
	Name       string  `json:"name"`
	Bytes      int64   `json:"bytes"`
	Percentage float64 `json:"percentage"`
}

type Result struct {
	Apps      []pack.DetectedApp `json:"apps"`
	Languages []Language         `json:"languages"`
	Infra     []pack.Tech        `json:"infra"`
}

func (c *Client) Detect(ctx context.Context, dir, sourceType, sourceRef string) (Result, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if res, err := c.DetectPath(ctx, abs); err == nil {
		return res, nil
	} else if res, zipErr := c.DetectZipDir(ctx, abs); zipErr == nil {
		return res, nil
	} else if sourceType == "github" && strings.TrimSpace(sourceRef) != "" {
		if res, ghErr := c.DetectGitHub(ctx, sourceRef); ghErr == nil {
			return res, nil
		}
		return Result{}, err
	} else {
		return Result{}, err
	}
}

func (c *Client) DetectPath(ctx context.Context, dir string) (Result, error) {
	body, _ := json.Marshal(map[string]string{"path": dir})
	return c.postJSON(ctx, "/v1/detect/path", body)
}

func (c *Client) DetectGitHub(ctx context.Context, repo string) (Result, error) {
	body, _ := json.Marshal(map[string]string{"url": repo})
	return c.postJSON(ctx, "/v1/detect/github", body)
}

func (c *Client) DetectZipDir(ctx context.Context, dir string) (Result, error) {
	var zipBuf bytes.Buffer
	if err := writeSourceZip(&zipBuf, dir); err != nil {
		return Result{}, err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "source.zip")
	if err != nil {
		return Result{}, err
	}
	if _, err := io.Copy(part, &zipBuf); err != nil {
		return Result{}, err
	}
	if err := writer.Close(); err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/detect/zip", &body)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return c.do(req)
}

func (c *Client) postJSON(ctx context.Context, path string, body []byte) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *Client) do(req *http.Request) (Result, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("detect status %d", resp.StatusCode)
	}
	var out Result
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, err
	}
	return out, nil
}

func writeSourceZip(w io.Writer, dir string) error {
	zw := zip.NewWriter(w)
	defer zw.Close()
	const maxZip = 70 << 20
	var written int64
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDetectDir(d.Name()) && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if written > maxZip {
			return fmt.Errorf("workspace too large to detect")
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil
		}
		header.Name = filepath.ToSlash(rel)
		header.Method = zip.Deflate
		dest, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return nil
		}
		n, copyErr := io.Copy(dest, io.LimitReader(src, 2<<20))
		_ = src.Close()
		written += n
		return copyErr
	})
}

func skipDetectDir(name string) bool {
	switch name {
	case "node_modules", ".git", "dist", "build", "out", "vendor", "target", ".next", ".turbo":
		return true
	default:
		return strings.HasPrefix(name, ".")
	}
}
