// Package userapi is the HTTP both plugins serve the same way: the plumbing, the
// routes for a proxy's users, and the views the page reads.
package userapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UIHeader is what the page sends on every API call.
const UIHeader = "X-MTProto-UI"

// Error is an answer with its own status code.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func Bad(format string, a ...any) error {
	return &Error{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

func Conflict(format string, a ...any) error {
	return &Error{http.StatusConflict, fmt.Sprintf(format, a...)}
}

func WriteJSON(w http.ResponseWriter, code int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return Bad("malformed request body")
	}
	return nil
}

func Health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok")
}

// UTC is t for JSON, or null when it is unset.
func UTC(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// SameSite keeps other sites' pages off the API: nothing here asks who you are.
// open are the page's own paths, stream the GET patterns an EventSource calls.
func SameSite(mux *http.ServeMux, open, stream map[string]bool, fail func(http.ResponseWriter, error)) http.Handler {
	refused := &Error{http.StatusForbidden, "requests from other sites are refused"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := (r.Method == http.MethodGet || r.Method == http.MethodHead) && open[r.URL.Path]
		if !page && !FromThisSite(r) {
			fail(w, refused)
			return
		}
		// no Sec-Fetch-Site: only the page sends UIHeader (EventSource can't, and a job holds no secrets)
		if !page && r.Header.Get("Sec-Fetch-Site") == "" && r.Header.Get(UIHeader) == "" {
			if _, pat := mux.Handler(r); r.Method != http.MethodGet || !stream[pat] {
				fail(w, refused)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// FromThisSite relies on Sec-Fetch-Site; browsers without it are left to the UIHeader check.
func FromThisSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
	default:
		return false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	// the panel rewrites Host and sets no X-Forwarded-Host, so usually there is nothing to compare
	fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")
	origin := r.Header.Get("Origin")
	if fwd = strings.TrimSpace(fwd); fwd == "" || origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, fwd)
}
