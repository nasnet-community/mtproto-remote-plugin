// Package ui serves the plugin's single-file web app.
package ui

import (
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
)

// The image build adds index.html.gz; the repo keeps only a text placeholder.
//
//go:embed index.html*
var files embed.FS

// Page is the embedded page, unpacked.
func Page() ([]byte, error) { return pick(files) }

// pick prefers the gzipped build over the placeholder.
func pick(fsys fs.FS) ([]byte, error) {
	gz, err := fs.ReadFile(fsys, "index.html.gz")
	if err != nil {
		return fs.ReadFile(fsys, "index.html")
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("ui: the embedded page is corrupt: %w", err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("ui: the embedded page is corrupt: %w", err)
	}
	return b, nil
}

// plain is ^[A-Za-z0-9._~-]+$ without regexp, which costs the binary more than it is worth here.
func plain(seg string) bool {
	if seg == "" {
		return false
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '_' || c == '~' || c == '-') {
			return false
		}
	}
	return true
}

// BasePath normalises BASE_PATH to "" or "/a/b"; it lands in the page, so only plain segments pass.
func BasePath(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" {
		return "", nil
	}
	if !strings.HasPrefix(s, "/") {
		return "", fmt.Errorf("BASE_PATH %q must start with /", s)
	}
	for _, seg := range strings.Split(s[1:], "/") {
		if !plain(seg) || seg == "." || seg == ".." {
			return "", fmt.Errorf("BASE_PATH %q is not a plain path", s)
		}
	}
	return s, nil
}

// Inject adds <base href="BASE/"> so relative api/ calls work whether the proxy strips the prefix or not.
func Inject(html []byte, base string) []byte {
	tag := []byte(`<head><base href="` + base + `/">`)
	return bytes.Replace(html, []byte("<head>"), tag, 1)
}

type handler struct{ plain, gz []byte }

// New serves the embedded page for base.
func New(base string) (http.Handler, error) {
	page, err := Page()
	if err != nil {
		return nil, err
	}
	return NewHandler(page, base), nil
}

// NewHandler serves html with the base tag, gzipped when the client takes it.
func NewHandler(html []byte, base string) http.Handler {
	h := handler{plain: Inject(html, base)}
	var b bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
	zw.Write(h.plain)
	zw.Close()
	h.gz = b.Bytes()
	return h
}

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("Content-Type", "text/html; charset=utf-8")
	hd.Set("Cache-Control", "no-store")
	hd.Set("Vary", "Accept-Encoding")
	body := h.plain
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		hd.Set("Content-Encoding", "gzip")
		body = h.gz
	}
	hd.Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

func acceptsGzip(h string) bool {
	for _, part := range strings.Split(h, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q, ok := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q=")
		if !ok {
			return true
		}
		v, err := strconv.ParseFloat(q, 64)
		return err == nil && v > 0
	}
	return false
}
