package ui

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedPageHasAHead(t *testing.T) {
	page, err := Page()
	if err != nil || !bytes.Contains(page, []byte("<head>")) {
		t.Fatalf("the embedded page has no <head> to put the base tag after: %v", err)
	}
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	zw.Write(b)
	zw.Close()
	return out.Bytes()
}

func TestPickPrefersTheGzippedBuild(t *testing.T) {
	placeholder := []byte("<html><head></head><body>placeholder</body></html>")
	built := []byte("<html><head></head><body>" + strings.Repeat("the app ", 100) + "</body></html>")

	got, err := pick(fstest.MapFS{"index.html": {Data: placeholder}})
	if err != nil || !bytes.Equal(got, placeholder) {
		t.Fatalf("placeholder only: %q, %v", got, err)
	}
	both := fstest.MapFS{"index.html": {Data: placeholder}, "index.html.gz": {Data: gzipped(t, built)}}
	got, err = pick(both)
	if err != nil || !bytes.Equal(got, built) {
		t.Fatalf("with the build: %q, %v", got, err)
	}
	// served the same as the plain page would be
	for _, ae := range []string{"", "gzip"} {
		a, b := get(NewHandler(got, "/x"), ae), get(NewHandler(built, "/x"), ae)
		if !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) || a.Header().Get("Content-Encoding") != b.Header().Get("Content-Encoding") {
			t.Fatalf("Accept-Encoding %q: the gzipped build is served differently", ae)
		}
	}
	if _, err := pick(fstest.MapFS{"index.html": {Data: placeholder}, "index.html.gz": {Data: []byte("not gzip")}}); err == nil {
		t.Fatal("a corrupt build was taken for the placeholder")
	}
	truncated := gzipped(t, built)
	if _, err := pick(fstest.MapFS{"index.html.gz": {Data: truncated[:len(truncated)/2]}}); err == nil {
		t.Fatal("a truncated build was accepted")
	}
}

var oldSegment = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

func TestPlainMatchesTheOldRegexp(t *testing.T) {
	for _, s := range []string{
		"", ".", "..", "a", "A-z_0.9~", "a b", "a/b", "a\n", "a\n\n", "\n", "é", "aé", "\xff", "a\x00", "%2e",
		"api", "telegram-mtproto", "~", "a;b", "a?b", "a#b", `a"b`, "<", strings.Repeat("x", 500),
	} {
		if got, want := plain(s), oldSegment.MatchString(s); got != want {
			t.Errorf("plain(%q) = %v, the regexp says %v", s, got, want)
		}
	}
}

func FuzzPlain(f *testing.F) {
	for _, s := range []string{"", "a", "a b", "é", "~.-_", "a\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := plain(s), oldSegment.MatchString(s); got != want {
			t.Fatalf("plain(%q) = %v, the regexp says %v", s, got, want)
		}
	})
}

func TestInject(t *testing.T) {
	page := []byte("<!doctype html><html><head><title>x</title></head><body><head></body></html>")
	got := string(Inject(page, "/api/plugin/view/telegram-mtproto"))
	want := `<head><base href="/api/plugin/view/telegram-mtproto/"><title>`
	if !strings.Contains(got, want) {
		t.Fatalf("got %s", got)
	}
	if strings.Count(got, "<base ") != 1 {
		t.Fatalf("injected more than once: %s", got)
	}
	if got := string(Inject(page, "")); !strings.Contains(got, `<head><base href="/"><title>`) {
		t.Fatalf("empty base path: %s", got)
	}
}

func TestBasePath(t *testing.T) {
	for in, want := range map[string]string{
		"":                                     "",
		"/":                                    "",
		" /api/plugin/view/telegram-mtproto/ ": "/api/plugin/view/telegram-mtproto",
		"/a/b":                                 "/a/b",
	} {
		got, err := BasePath(in)
		if err != nil || got != want {
			t.Errorf("BasePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"api/view", `/a"><script>`, "/a b", "/a//b", "/a/../b"} {
		if _, err := BasePath(bad); err == nil {
			t.Errorf("BasePath(%q) was accepted", bad)
		}
	}
}

func get(h http.Handler, acceptEncoding string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if acceptEncoding != "" {
		r.Header.Set("Accept-Encoding", acceptEncoding)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHandler(t *testing.T) {
	page := []byte("<html><head></head><body>" + strings.Repeat("hello ", 200) + "</body></html>")
	h := NewHandler(page, "/base")

	plain := get(h, "")
	if plain.Code != 200 || plain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("plain: %d %v", plain.Code, plain.Header())
	}
	if !strings.Contains(plain.Body.String(), `<base href="/base/">`) {
		t.Fatal("no base tag")
	}
	if cc := plain.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if ct := plain.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if v := plain.Header().Get("Vary"); v != "Accept-Encoding" {
		t.Errorf("Vary = %q", v)
	}

	gz := get(h, "br, gzip;q=0.8")
	if gz.Header().Get("Content-Encoding") != "gzip" || gz.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("gzip: %v", gz.Header())
	}
	zr, err := gzip.NewReader(gz.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if string(body) != plain.Body.String() {
		t.Fatal("the gzip body is not the page")
	}
	if gz.Body.Len() >= plain.Body.Len() {
		t.Error("gzip did not shrink a repetitive page")
	}

	for _, ae := range []string{"identity", "gzip;q=0", "br"} {
		if w := get(h, ae); w.Header().Get("Content-Encoding") != "" {
			t.Errorf("Accept-Encoding %q got gzip", ae)
		}
	}
}
