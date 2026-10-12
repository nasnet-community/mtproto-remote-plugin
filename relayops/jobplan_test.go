package relayops

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/installer"
)

func names(steps []installer.Step) string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Name+":"+s.State)
	}
	return strings.Join(out, " ")
}

func TestAPlannedJobListsWhatIsLeftAndPlacesSurprisesInOrder(t *testing.T) {
	s := NewService(ServiceConfig{})
	j := s.newJob("install", named("Frankfurt"), planned("check", "verify", "account", "upload"))
	if got := names(j.snapshot().Steps); got != "check:pending verify:pending account:pending upload:pending" {
		t.Fatalf("planned: %s", got)
	}
	j.step(installer.Step{Name: "check", State: "ok"})
	j.step(installer.Step{Name: "verify", State: "ok"})
	j.step(installer.Step{Name: "nftables", State: "running"}) // not planned: lands before what is left
	if got := names(j.snapshot().Steps); got != "check:ok verify:ok nftables:running account:pending upload:pending" {
		t.Fatalf("after a surprise step: %s", got)
	}
	if j.snapshot().Target != "Frankfurt" {
		t.Errorf("target = %q", j.snapshot().Target)
	}

	// a failed job keeps what it never reached; a successful one drops what it skipped
	failed := s.newJob("repair", planned("check", "upload"))
	failed.step(installer.Step{Name: "check", State: "failed"})
	failed.finish(errors.New("boom"))
	if got := names(failed.snapshot().Steps); got != "check:failed upload:pending" {
		t.Errorf("failed job: %s", got)
	}
	ok := s.newJob("uninstall", planned("remove", "forget"))
	ok.step(installer.Step{Name: "forget", State: "ok"})
	ok.finish(nil)
	if got := names(ok.snapshot().Steps); got != "forget:ok" {
		t.Errorf("finished job: %s", got)
	}
}

func TestJobsListsRunningAndRecentNewestFirst(t *testing.T) {
	s := NewService(ServiceConfig{})
	old := s.newJob("rotate")
	old.finish(nil)
	long := time.Now().Add(-2 * time.Hour)
	old.v.FinishedAt, old.v.StartedAt = &long, long.Add(-time.Minute)
	first := s.newJob("update-agent")
	first.v.StartedAt = time.Now().Add(-time.Minute)
	second := s.newJob("install")
	got := s.Jobs(20)
	if len(got) != 2 || got[0].ID != second.v.ID || got[1].ID != first.v.ID {
		t.Fatalf("jobs: %+v", got)
	}
	if len(s.Jobs(1)) != 1 {
		t.Error("the limit is ignored")
	}
}

func TestUploadWaitGrowsWithTheFile(t *testing.T) {
	if uploadWait(1<<20) != 5*time.Minute {
		t.Error("a small file should get the 5 minute floor")
	}
	// 15.4 MB at ~20 KB/s
	if d := uploadWait(15_443_096); d < 12*time.Minute || d > 13*time.Minute {
		t.Errorf("telemt gets %v", d)
	}
}
