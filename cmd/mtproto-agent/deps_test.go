package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The agent's bytes must not depend on the Telegram address list: the list
// ships after the agent release and changes without one.
func TestTheAgentDoesNotCarryTheAddressList(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	cmd := exec.Command(gobin, "list", "-deps", "-f", "{{.ImportPath}} {{join .EmbedFiles \" \"}}", ".")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg, embeds, _ := strings.Cut(line, " ")
		if strings.Contains(embeds, "pinned_cidr.txt") || strings.HasSuffix(pkg, "/tgnet") {
			t.Errorf("the agent links %s (embeds %q): the address list would change its bytes", pkg, embeds)
		}
	}
}
