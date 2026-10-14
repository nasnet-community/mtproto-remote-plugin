package release

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The pins describe an agent built at an earlier tag. Since the agent does not
// import this package, any later commit must rebuild to the very same bytes when
// stamped with the pinned version. The module release runs this with CHECK_PINS=1.
func TestPinnedAgentRebuildsByteForByte(t *testing.T) {
	if os.Getenv("CHECK_PINS") != "1" {
		t.Skip("set CHECK_PINS=1 to rebuild the agent and compare it with the pins")
	}
	if AgentVersion == "0.0.0-dev" {
		t.Skip("no agent release is pinned yet")
	}
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []Arch{AMD64, ARM64} {
		pin, err := Lookup(Agent, arch)
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "agent")
		cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false",
			"-ldflags", "-s -w -X main.version="+AgentVersion, "-o", out, "./cmd/mtproto-agent")
		cmd.Dir = strings.TrimSpace(string(root))
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+string(arch))
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: build failed: %v\n%s", arch, err, b)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != pin.SHA256 {
			t.Errorf("%s: the agent now builds to %s, the pin says %s: the agent changed since agent-v%s, cut a new agent release", arch, got, pin.SHA256, AgentVersion)
		}
	}
}
