package tgnet

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

//go:embed pinned_cidr.txt
var pinnedCIDR string

// placeholderMark is in the shipped placeholder file; the release gate looks
// for it.
const placeholderMark = "PLACEHOLDER"

// DefaultURL is where Telegram publishes its datacentre ranges.
const DefaultURL = "https://core.telegram.org/resources/cidr.txt"

// ListURL turns the configured list address into the one to fetch: empty means
// Telegram's own list, "off" turns the refresh off.
func ListURL(setting string) string {
	v := strings.TrimSpace(setting)
	switch {
	case v == "":
		return DefaultURL
	case strings.EqualFold(v, "off"):
		return ""
	}
	return v
}

// PinnedPrefixes parses the list compiled into the binary.
func PinnedPrefixes() ([]netip.Prefix, error) { return parsePinned(pinnedCIDR) }

func parsePinned(list string) ([]netip.Prefix, error) {
	ps, err := ParseList(strings.NewReader(list))
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if err := checkPublic(p); err != nil {
			return nil, err
		}
	}
	return ps, nil
}

// PinnedIsPlaceholder reports whether the embedded list is still the stand-in.
func PinnedIsPlaceholder() bool { return strings.Contains(pinnedCIDR, placeholderMark) }

// Refresher keeps the datacentre allowlist current. The pinned list is the
// floor; a daily fetch can only add to it; a failed or implausible fetch leaves
// everything as it was.
type Refresher struct {
	List *Allowlist

	// URL returns the list address to fetch (empty disables the refresh); it is a
	// function so the setting can change without a restart.
	URL func() string
	// Client returns the HTTP client for the fetch; it must leave through the
	// foreign exit.
	Client func() *http.Client
	// Changed is called after a refresh that altered the list, so agents get the
	// new prefixes.
	Changed func()

	Every time.Duration // default 24 h
	First time.Duration // delay before the first attempt (default 2 min)

	mu      sync.Mutex
	lastErr string
	lastOK  time.Time
}

// NewRefresher builds the service around the pinned list.
func NewRefresher() (*Refresher, error) {
	pinned, err := PinnedPrefixes()
	if err != nil {
		return nil, err
	}
	return &Refresher{List: New(pinned)}, nil
}

// Status is for the UI: when the list last refreshed and why not.
func (s *Refresher) Status() (lastOK time.Time, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastOK, s.lastErr
}

// Run fetches on a schedule until ctx ends.
func (s *Refresher) Run(ctx context.Context) {
	first, every := s.First, s.Every
	if first == 0 {
		first = 2 * time.Minute
	}
	if every == 0 {
		every = 24 * time.Hour
	}
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_ = s.RefreshNow(ctx)
		t.Reset(every)
	}
}

// RefreshNow fetches once.
func (s *Refresher) RefreshNow(ctx context.Context) error {
	url := ""
	if s.URL != nil {
		url = s.URL()
	}
	if url == "" || s.Client == nil {
		return nil
	}
	before := len(s.List.Prefixes())
	n, err := s.List.Refresh(ctx, func(ctx context.Context) (io.ReadCloser, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.Client().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, &statusError{resp.StatusCode}
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	})
	s.mu.Lock()
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr, s.lastOK = "", time.Now()
	}
	s.mu.Unlock()
	if err == nil && (n > 0 || len(s.List.Prefixes()) != before) && s.Changed != nil {
		s.Changed()
	}
	return err
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "the list server answered " + http.StatusText(e.code) }
