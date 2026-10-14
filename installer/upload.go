package installer

import (
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"sync"
	"time"
)

// upload counts one step's bytes and reports them on the step about once a second,
// so a slow link shows how far it got instead of looking stuck.
type upload struct {
	step     string
	total    int64 // the files' own size; compressed, less than this goes over the wire
	gzip     bool
	progress Progress
	start    time.Time

	mu   sync.Mutex
	read int64 // file bytes consumed
	sent int64 // bytes handed to the connection
	last time.Time
}

func (u *upload) counted(r io.Reader) io.Reader { return readCounter{r, u} }

type readCounter struct {
	r io.Reader
	u *upload
}

func (c readCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.u.add(int64(n), 0)
	}
	return n, err
}

type wireCounter struct {
	w io.Writer
	u *upload
}

func (c wireCounter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.u.add(0, int64(n))
	}
	return n, err
}

// gzipped streams r compressed; closing the returned reader stops the compressor.
func (u *upload) gzipped(r io.Reader) *io.PipeReader {
	pr, pw := io.Pipe()
	go func() {
		zw, _ := gzip.NewWriterLevel(wireCounter{pw, u}, gzip.BestCompression)
		_, err := io.Copy(zw, r)
		if cerr := zw.Close(); err == nil {
			err = cerr
		}
		pw.CloseWithError(err)
	}()
	return pr
}

func (u *upload) add(read, sent int64) {
	u.mu.Lock()
	u.read += read
	if u.gzip {
		u.sent += sent
	} else {
		u.sent += read
	}
	now := time.Now()
	if u.progress == nil || (now.Sub(u.last) < time.Second && u.read < u.total) {
		u.mu.Unlock()
		return
	}
	u.last = now
	detail := u.detailLocked(now)
	u.mu.Unlock()
	u.progress(Step{Name: u.step, State: "running", Detail: detail})
}

// detailLocked reads like "38% · 3.9 MB sent · about 2 min left".
func (u *upload) detailLocked(now time.Time) string {
	pct := int64(100)
	if u.total > 0 {
		pct = min(u.read*100/u.total, 100)
	}
	s := fmt.Sprintf("%d%% · %s sent", pct, size(u.sent))
	// the first seconds say little about the rate
	if el := now.Sub(u.start); el >= 3*time.Second && u.read > 0 && u.read < u.total {
		left := time.Duration(float64(el) * float64(u.total-u.read) / float64(u.read))
		s += " · about " + roughly(left) + " left"
	}
	return s
}

func size(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", (n+512)>>10)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// roughly rounds up: a promise of "1 min" that takes two is worse than the reverse.
func roughly(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(math.Ceil(d.Seconds()/5))*5)
	}
	return fmt.Sprintf("%d min", int(math.Ceil(d.Minutes())))
}
