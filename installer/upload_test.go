package installer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUploadDetailReadsLikeProgress(t *testing.T) {
	t0 := time.Now()
	u := &upload{total: 100 << 20, start: t0, read: 38 << 20, sent: 4089446} // 3.9 MB
	if got, want := u.detailLocked(t0.Add(time.Minute)), "38% · 3.9 MB sent · about 2 min left"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// no guess in the first seconds, kilobytes below a megabyte
	u.read, u.sent = 1<<20, 300<<10
	if got, want := u.detailLocked(t0.Add(2*time.Second)), "1% · 300 KB sent"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := roughly(12 * time.Second); got != "15 s" {
		t.Errorf("roughly(12s) = %q", got)
	}
}

func TestCompressedUploadArrivesWholeAndReportsProgress(t *testing.T) {
	gz, err := exec.LookPath("gzip")
	if err != nil {
		t.Skip("no gzip on this machine")
	}
	h := startFakeHost(t)
	os.Symlink(gz, filepath.Join(h.bin, "gzip"))
	req := request(t)
	big := strings.Repeat("telemt-binary-bytes ", 50000) // compresses well, so the wire count is far below the file's
	req.Telemt = bin(big)
	var details []string
	_, err = Install(context.Background(), connect(t, h), req, func(s Step) {
		if s.Name == "upload" && s.State == "running" && s.Detail != "" {
			details = append(details, s.Detail)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.read("usr/local/bin/telemt"); got != big {
		t.Fatalf("telemt arrived as %d bytes, want %d", len(got), len(big))
	}
	if got := h.read("usr/local/bin/mtproto-agent"); got != fakeAgent {
		t.Errorf("agent binary: %q", got)
	}
	compressed := false
	for _, c := range h.commands() {
		compressed = compressed || strings.Contains(c, "gzip -dc > ")
	}
	if !compressed {
		t.Error("the upload did not go compressed although the box has gzip")
	}
	if len(details) == 0 || !strings.HasPrefix(details[len(details)-1], "100% · ") || !strings.Contains(details[len(details)-1], "KB sent") {
		t.Errorf("progress details: %q", details)
	}
}

func TestAFailedAgentUploadSkipsTheProxy(t *testing.T) {
	h := startFakeHost(t)
	h.env["FAKE_BAD_SHA"] = "1"
	if _, err := Install(context.Background(), connect(t, h), request(t), nil); err == nil {
		t.Fatal("a corrupted upload succeeded")
	}
	for _, c := range h.commands() {
		if strings.Contains(c, "telemt.upload") {
			t.Fatalf("the proxy was still uploaded after the agent failed: %q", c)
		}
	}
}
