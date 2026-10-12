package relayops_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops/relaytest"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
)

func TestInstallRegistersTheNodeAndWaitsForTheLink(t *testing.T) {
	r := relaytest.NewRig()
	r.Links.SetUp(1, true)
	id, err := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
	if err != nil {
		t.Fatal(err)
	}
	v := r.Wait(t, id)
	if v.State != relayops.JobOK || v.NodeID != 1 {
		t.Fatalf("job = %+v", v)
	}
	row, err := r.Store.Get(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if row.Disguise != "mirror.example.com" || row.AgentID != "agent-xyz" || row.HubPort != 8443 || row.HostKey != "SHA256:abc" || row.SSHUser != "root" || len(row.Token) != 64 || len(row.RealityPub) != 64 || len(row.ShortID) != 16 {
		t.Fatalf("row = %+v", row)
	}
	if len(r.Links.Ensured) != 1 || r.Admin.Names[1] != "Relay 1" {
		t.Fatalf("ensured=%v nodes=%+v", r.Links.Ensured, r.Admin.Names)
	}
	names := map[string]string{}
	for _, s := range v.Steps {
		names[s.Name] = s.State
	}
	for _, want := range []string{"download", "check", "service", "register", "link"} {
		if names[want] != "ok" {
			t.Errorf("step %s = %q (steps %+v)", want, names[want], v.Steps)
		}
	}
}

// Whatever the job view says, it must never carry the operator's SSH secrets.
func TestJobViewsNeverCarryCredentials(t *testing.T) {
	r := relaytest.NewRig()
	r.Links.SetUp(1, true)
	p := relaytest.GoodInstall()
	p.Login.Key = []byte("-----BEGIN PRIVATE KEY-----\nTOPSECRETKEYMATERIAL\n")
	id, _ := r.Svc.StartInstall(context.Background(), p)
	v := r.Wait(t, id)
	b, _ := json.Marshal(v)
	for _, secret := range []string{"hunter2-super-secret", "TOPSECRETKEYMATERIAL"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("a secret reached the job view: %s", b)
		}
	}
}

func TestInstallValidation(t *testing.T) {
	r := relaytest.NewRig()
	cases := map[string]func(*relayops.InstallParams){
		"no host":              func(p *relayops.InstallParams) { p.Login.Host = "" },
		"bad host":             func(p *relayops.InstallParams) { p.Login.Host = "a b;c" },
		"no credentials":       func(p *relayops.InstallParams) { p.Login.Password = "" },
		"unconfirmed host key": func(p *relayops.InstallParams) { p.Login.HostKey = "" },
		"no name":              func(p *relayops.InstallParams) { p.Name = " " },
		"bad domain":           func(p *relayops.InstallParams) { p.Disguise = "not a domain" },
		"too few streams":      func(p *relayops.InstallParams) { p.MaxStreams = 5 },
		"ports clash":          func(p *relayops.InstallParams) { p.HubPort, p.CapturePort = 9000, 9000 },
		"privileged port":      func(p *relayops.InstallParams) { p.HubPort = 80 },
		"ssh port":             func(p *relayops.InstallParams) { p.HubPort = 22 },
	}
	for name, mutate := range cases {
		p := relaytest.GoodInstall()
		mutate(&p)
		if _, err := r.Svc.StartInstall(context.Background(), p); !errors.Is(err, relayops.ErrInvalid) {
			t.Errorf("%s: err = %v, want relayops.ErrInvalid", name, err)
		}
	}
	if len(r.SSH.Targets) != 0 {
		t.Fatal("a rejected request reached SSH")
	}
}

// A bad cover site is refused by the request itself: no job, no download, nothing on the box.
func TestABadCoverSiteIsRefusedBeforeTheInstallStarts(t *testing.T) {
	r := relaytest.NewRig()
	r.SSH.Cover = &ctrl.DisguiseResult{TLS13: true, X25519: true, Err: "the site does not offer h2"}
	id, err := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
	var ce *relayops.CoverError
	if !errors.As(err, &ce) || id != "" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if ce.Domain != "mirror.example.com" || ce.Result.H2 || !strings.Contains(err.Error(), "does not work as a cover site") || !strings.Contains(err.Error(), "h2") {
		t.Fatalf("err = %v (%+v)", err, ce.Result)
	}
	if len(r.SSH.Installed) != 0 || len(r.Admin.Names) != 0 {
		t.Fatal("the install went ahead")
	}
}

// The site was tested before the job, so the job says so and the agent doesn't test it again.
func TestATestedCoverSiteIsNotTestedAgain(t *testing.T) {
	r := relaytest.NewRig()
	r.Links.SetUp(1, true)
	id, err := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
	if err != nil {
		t.Fatal(err)
	}
	v := r.Wait(t, id)
	if v.State != relayops.JobOK || len(r.SSH.Installed) != 1 || !r.SSH.Installed[0].CoverChecked {
		t.Fatalf("job = %+v installed = %+v", v, r.SSH.Installed)
	}
	if !slices.Equal(r.SSH.CoverChecks, []string{"mirror.example.com"}) {
		t.Fatalf("cover checks = %v", r.SSH.CoverChecks)
	}
	if first := v.Steps[0]; first.Name != "disguise" || first.State != "ok" || first.Detail != "mirror.example.com" {
		t.Fatalf("steps = %+v", v.Steps)
	}
}

// A box that can't be asked before the upload is tested by the agent after it, as before.
func TestABoxThatCannotTestEarlyLeavesItToTheAgent(t *testing.T) {
	r := relaytest.NewRig()
	r.Links.SetUp(1, true)
	r.SSH.CoverUnchecked = true
	id, err := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
	if err != nil {
		t.Fatal(err)
	}
	v := r.Wait(t, id)
	if v.State != relayops.JobOK || r.SSH.Installed[0].CoverChecked {
		t.Fatalf("job = %+v installed = %+v", v, r.SSH.Installed)
	}
	for _, s := range v.Steps {
		if s.Name == "disguise" {
			t.Fatalf("the job claims a test that never ran: %+v", v.Steps)
		}
	}
}

// A wrong password (or an unreachable VPS) also comes back with the request, text intact.
func TestALoginThatFailsComesBackBeforeTheInstall(t *testing.T) {
	r := relaytest.NewRig()
	r.SSH.CoverErr = errors.New("installer: login: ssh: unable to authenticate")
	id, err := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
	if !errors.Is(err, relayops.ErrVPS) || err.Error() != "installer: login: ssh: unable to authenticate" || id != "" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if len(r.SSH.Installed) != 0 {
		t.Fatal("the install went ahead")
	}
}

// Installing anyway skips the early test; the agent's own test then only warns.
func TestAllowingAnUnverifiedCoverSkipsTheEarlyTest(t *testing.T) {
	r := relaytest.NewRig()
	r.Links.SetUp(1, true)
	r.SSH.Cover = &ctrl.DisguiseResult{Err: "the site does not offer h2"}
	p := relaytest.GoodInstall()
	p.AllowUnverifiedDisguise = true
	id, err := r.Svc.StartInstall(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, id); v.State != relayops.JobOK || len(r.SSH.CoverChecks) != 0 {
		t.Fatalf("job = %+v checks = %v", v, r.SSH.CoverChecks)
	}
}

// Another job on the box refuses the request before it logs in.
func TestABusyHostIsRefusedBeforeTheCoverTest(t *testing.T) {
	r := relaytest.NewRig()
	r.SSH.Block = true
	if _, err := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall()); !errors.Is(err, relayops.ErrBusy) {
		t.Fatalf("err = %v", err)
	}
	if len(r.SSH.CoverChecks) != 1 {
		t.Fatalf("cover checks = %v", r.SSH.CoverChecks)
	}
}

func TestAFailedInstallLeavesNothingBehind(t *testing.T) {
	r := relaytest.NewRig()
	r.SSH.InstallErr = errors.New("the agent did not start listening on port 8443")
	id, _ := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
	v := r.Wait(t, id)
	if v.State != relayops.JobFailed || !strings.Contains(v.Error, "port 8443") {
		t.Fatalf("job = %+v", v)
	}
	if len(r.Admin.Names) != 0 {
		t.Fatal("a node was registered for a failed install")
	}
	if rows, _ := r.Store.List(context.Background()); len(rows) != 0 {
		t.Fatal("a row was saved for a failed install")
	}
	// and the VPS: the half-install is taken away again
	if r.SSH.Uninstalls != 1 {
		t.Fatalf("uninstalls = %d: binaries, the user and a state no panel knows stay on the box", r.SSH.Uninstalls)
	}
}

// Only what this job put there is taken away: not an agent that was there
// before, and nothing at all when the checks refused before any change.
func TestAFailedInstallLeavesAnEarlierAgentAlone(t *testing.T) {
	for name, tweak := range map[string]func(*relaytest.SSH){
		"an agent was there": func(f *relaytest.SSH) { f.WasInstalled = true },
		"refused in checks":  func(f *relaytest.SSH) { f.FailEarly = true },
	} {
		r := relaytest.NewRig()
		r.SSH.InstallErr = errors.New("port 443 is already in use on the VPS")
		tweak(r.SSH)
		id, _ := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
		if v := r.Wait(t, id); v.State != relayops.JobFailed {
			t.Fatalf("%s: job = %+v", name, v)
		}
		if r.SSH.Uninstalls != 0 {
			t.Errorf("%s: the box was uninstalled", name)
		}
	}
}

// A host that never finishes ends the job instead of holding it for ever.
func TestJobsHaveADeadline(t *testing.T) {
	r := relaytest.NewRig(func(c *relayops.ServiceConfig) { c.JobTimeout = 300 * time.Millisecond })
	r.SSH.Block = true
	start := time.Now()
	id, _ := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
	v := r.Wait(t, id)
	if v.State != relayops.JobFailed || time.Since(start) > 5*time.Second {
		t.Fatalf("job = %+v after %v", v, time.Since(start))
	}
}

func TestIfThePanelCannotRememberTheNodeTheVPSIsCleanedUp(t *testing.T) {
	r := relaytest.NewRig()
	r.Admin.CreateE = errors.New("database is locked")
	id, _ := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
	v := r.Wait(t, id)
	if v.State != relayops.JobFailed || r.SSH.Uninstalls != 1 {
		t.Fatalf("state=%s uninstalls=%d", v.State, r.SSH.Uninstalls)
	}
}

func TestAnAgentThatNeverAnswersFailsTheJobButKeepsTheNodeForRepair(t *testing.T) {
	r := relaytest.NewRig() // the link never comes up
	id, _ := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
	v := r.Wait(t, id)
	if v.State != relayops.JobFailed || !strings.Contains(v.Error, "TCP port 8443") {
		t.Fatalf("job = %+v", v)
	}
	if _, err := r.Store.Get(context.Background(), 1); err != nil {
		t.Fatalf("the node was dropped: %v", err)
	}
}

func TestRepairAdoptsWhatIsOnTheBox(t *testing.T) {
	r := relaytest.NewRig()
	r.Links.SetUp(1, true)
	r.Links.RedialUp = true
	id, _ := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
	r.Wait(t, id)
	before, _ := r.Store.Get(context.Background(), 1)

	// the box has different credentials than the panel remembers (a rotation whose reply was lost)
	other, _ := installer.NewCreds()
	r.SSH.Result = &installer.Result{AgentID: before.AgentID, Creds: other, HubPort: 8443, CapturePort: 9443}
	jid, err := r.Svc.StartRepair(context.Background(), 1, relayops.Login{Password: "pw"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, jid); v.State != relayops.JobOK {
		t.Fatalf("repair = %+v", v)
	}
	after, _ := r.Store.Get(context.Background(), 1)
	if after.Token == before.Token || after.RealityPub == before.RealityPub {
		t.Fatal("the credentials on the box were not adopted")
	}
	if len(r.Links.CredsSet) != 1 || r.Links.CredsSet[0].Token != after.Token {
		t.Fatalf("the running link was not given the new credentials: %+v", r.Links.CredsSet)
	}
	last := r.SSH.Installed[len(r.SSH.Installed)-1]
	if !last.KeepState {
		t.Fatal("a plain repair must keep the credentials on the box")
	}
	// with no port or host key given, the stored ones are used; the address always is
	tgt := r.SSH.Targets[len(r.SSH.Targets)-1]
	if tgt.Host != "203.0.113.9" || tgt.HostKey != "SHA256:abc" || tgt.Port != 22 {
		t.Fatalf("target = %+v", tgt)
	}
	// reset reinstalls with fresh credentials
	jid, _ = r.Svc.StartRepair(context.Background(), 1, relayops.Login{Password: "pw"}, true)
	r.Wait(t, jid)
	if last := r.SSH.Installed[len(r.SSH.Installed)-1]; last.KeepState {
		t.Fatal("reset must not keep the old credentials")
	}
}

// A repair is done when the link is back on what it adopted, not before.
func TestRepairWaitsForTheLink(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.RedialUp = false // the agent never answers again
	jid, err := r.Svc.StartRepair(context.Background(), 1, relayops.Login{Password: "pw"}, false)
	if err != nil {
		t.Fatal(err)
	}
	v := r.Wait(t, jid)
	if v.State != relayops.JobFailed || !strings.Contains(v.Error, "TCP port 8443") {
		t.Fatalf("repair = %+v", v)
	}
	if r.Links.Redials == 0 {
		t.Fatal("the running link was never redialled with what the repair adopted")
	}
}

// sshd may have moved: repair and remove use the port and host key the caller
// confirmed, and a repair that worked remembers them.
func TestRepairAndRemoveUseTheGivenPortAndHostKey(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.RedialUp = true
	jid, err := r.Svc.StartRepair(context.Background(), 1, relayops.Login{Port: 2222, HostKey: "SHA256:moved", Password: "pw"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, jid); v.State != relayops.JobOK {
		t.Fatalf("repair = %+v", v)
	}
	if tgt := r.SSH.Targets[len(r.SSH.Targets)-1]; tgt.Port != 2222 || tgt.HostKey != "SHA256:moved" {
		t.Fatalf("target = %+v", tgt)
	}
	row, _ := r.Store.Get(context.Background(), 1)
	if row.SSHPort != 2222 || row.HostKey != "SHA256:moved" {
		t.Fatalf("not remembered: port %d key %s", row.SSHPort, row.HostKey)
	}
	r.Links.SetUp(1, false)
	jid, err = r.Svc.StartUninstall(context.Background(), 1, &relayops.Login{Port: 2200, HostKey: "SHA256:again", Password: "pw"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, jid); v.State != relayops.JobOK {
		t.Fatalf("remove = %+v", v)
	}
	if tgt := r.SSH.Targets[len(r.SSH.Targets)-1]; tgt.Port != 2200 || tgt.HostKey != "SHA256:again" {
		t.Fatalf("remove target = %+v", tgt)
	}
}

// Router mode decides how SSH leaves the panel: every dial the service makes
// goes through the configured one.
func TestEverySSHConnectionUsesTheConfiguredDialer(t *testing.T) {
	var used atomic.Int32
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		used.Add(1)
		return nil, errors.New("not in tests")
	}
	r := relaytest.NewRig(func(c *relayops.ServiceConfig) { c.Dial = dial })
	r.Svc.Fingerprint(context.Background(), "203.0.113.9", 22, "root")
	r.Installed(t)
	r.Links.RedialUp = true
	jid, _ := r.Svc.StartRepair(context.Background(), 1, relayops.Login{Password: "pw"}, false)
	r.Wait(t, jid)
	r.Links.SetUp(1, false)
	jid, _ = r.Svc.StartUninstall(context.Background(), 1, &relayops.Login{Password: "pw"}, false)
	r.Wait(t, jid)
	if len(r.SSH.Targets) < 5 {
		t.Fatalf("only %d SSH targets", len(r.SSH.Targets))
	}
	for i, tgt := range r.SSH.Targets {
		if tgt.Dial == nil {
			t.Fatalf("target %d has no dialer", i)
		}
		before := used.Load()
		tgt.Dial(context.Background(), "tcp", "203.0.113.9:22")
		if used.Load() != before+1 {
			t.Fatalf("target %d dials some other way", i)
		}
	}
}

// One job per node: an update, a rotation, a repair and a removal at once
// would step on each other on the box.
func TestOneJobPerNodeAtATime(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	release := make(chan struct{})
	r.Links.Mu.Lock()
	r.Links.AfterRaw = func() { <-release }
	r.Links.Mu.Unlock()
	id, err := r.Svc.StartUpdate(context.Background(), 1, "telemt")
	if err != nil {
		t.Fatal(err)
	}
	for name, start := range map[string]func() error{
		"update": func() error { _, err := r.Svc.StartUpdate(context.Background(), 1, "agent"); return err },
		"rotate": func() error { _, err := r.Svc.StartRotate(context.Background(), 1); return err },
		"repair": func() error {
			_, err := r.Svc.StartRepair(context.Background(), 1, relayops.Login{Password: "pw"}, false)
			return err
		},
		"uninstall": func() error { _, err := r.Svc.StartUninstall(context.Background(), 1, nil, false); return err },
	} {
		if err := start(); !errors.Is(err, relayops.ErrBusy) {
			t.Errorf("%s during an update: %v, want ErrBusy", name, err)
		}
	}
	close(release)
	if v := r.Wait(t, id); v.State != relayops.JobOK {
		t.Fatalf("update = %+v", v)
	}
	if _, err := r.Svc.StartRotate(context.Background(), 1); err != nil {
		t.Fatalf("a job after the first finished: %v", err)
	}
}

// With a login, the removal over the link is followed by one over SSH: the
// agent's own teardown cannot take everything (its unit, under systemd).
func TestUninstallWithALoginAlsoCleansUpOverSSH(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.DownAfterUninstall = true
	jid, err := r.Svc.StartUninstall(context.Background(), 1, &relayops.Login{Password: "pw"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, jid); v.State != relayops.JobOK {
		t.Fatalf("job = %+v", v)
	}
	if r.Links.CallsOf(ctrl.MUninstall) != 1 || r.SSH.Uninstalls != 1 {
		t.Fatalf("link removals %d, ssh removals %d", r.Links.CallsOf(ctrl.MUninstall), r.SSH.Uninstalls)
	}
}

func TestUninstallThroughTheLinkNeedsNoLogin(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	jid, err := r.Svc.StartUninstall(context.Background(), 1, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, jid); v.State != relayops.JobOK {
		t.Fatalf("job = %+v", v)
	}
	if len(r.Links.Calls) != 1 || r.Links.Calls[0] != ctrl.MUninstall || r.SSH.Uninstalls != 0 {
		t.Fatalf("calls=%v ssh uninstalls=%d", r.Links.Calls, r.SSH.Uninstalls)
	}
	if _, err := r.Store.Get(context.Background(), 1); !errors.Is(err, relayops.ErrNotFound) || len(r.Admin.Deleted) != 1 || len(r.Links.Removed) != 1 {
		t.Fatalf("not forgotten: err=%v deleted=%v removed=%v", err, r.Admin.Deleted, r.Links.Removed)
	}
}

func TestUninstallWithTheLinkDownNeedsALoginOrForce(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.SetUp(1, false)
	if _, err := r.Svc.StartUninstall(context.Background(), 1, nil, false); !errors.Is(err, relayops.ErrNeedLogin) {
		t.Fatalf("err = %v", err)
	}
	// with a login it goes over SSH
	jid, err := r.Svc.StartUninstall(context.Background(), 1, &relayops.Login{Password: "pw"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, jid); v.State != relayops.JobOK || r.SSH.Uninstalls != 1 {
		t.Fatalf("job=%+v ssh=%d", v, r.SSH.Uninstalls)
	}
}

func TestForceDeleteForgetsTheNodeEvenIfTheAgentStays(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.SetUp(1, false)
	jid, err := r.Svc.StartUninstall(context.Background(), 1, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, jid); v.State != relayops.JobOK {
		t.Fatalf("job = %+v", v)
	}
	if _, err := r.Store.Get(context.Background(), 1); !errors.Is(err, relayops.ErrNotFound) {
		t.Fatal("the row survived a forced delete")
	}
}

func TestAFailedUninstallKeepsTheNode(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.CallErr = errors.New("agent did not answer")
	jid, _ := r.Svc.StartUninstall(context.Background(), 1, nil, false)
	v := r.Wait(t, jid)
	if v.State != relayops.JobFailed {
		t.Fatalf("job = %+v", v)
	}
	if _, err := r.Store.Get(context.Background(), 1); err != nil {
		t.Fatalf("the node was forgotten although the agent is still there: %v", err)
	}
}

func TestFingerprintValidatesAndPassesThrough(t *testing.T) {
	r := relaytest.NewRig()
	fp, err := r.Svc.Fingerprint(context.Background(), "203.0.113.9", 0, "")
	if err != nil || fp != "SHA256:abc" {
		t.Fatalf("%q %v", fp, err)
	}
	if _, err := r.Svc.Fingerprint(context.Background(), "a b", 22, "root"); !errors.Is(err, relayops.ErrInvalid) {
		t.Fatalf("bad host: %v", err)
	}
}

// --- day-2 operations ---

func TestInstallRecordsTheArchitectureForLaterUpdates(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
}

func TestTelemtUpdateUploadsThePinnedBinary(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	id, err := r.Svc.StartUpdate(context.Background(), 1, "telemt")
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, id); v.State != relayops.JobOK {
		t.Fatalf("job = %+v", v)
	}
	if len(r.Links.RawReqs) != 1 || r.Links.RawReqs[0].Kind != "telemt" || r.Links.RawReqs[0].Arch != "amd64" || r.Links.RawReqs[0].SHA256 != strings.Repeat("a", 64) || r.Links.RawReqs[0].Ver != release.TelemtVersion {
		t.Fatalf("request = %+v", r.Links.RawReqs)
	}
	for _, m := range r.Links.Calls {
		if m == ctrl.MUpdateCommit {
			t.Fatal("telemt updates verify themselves: there is nothing to commit")
		}
	}
}

func TestAgentUpdateWaitsForTheNewVersionThenCommits(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.Mu.Lock()
	r.Links.AgentVersion, r.Links.BootID = "old", "boot-1"
	r.Links.AfterRaw = func() {
		go func() {
			time.Sleep(300 * time.Millisecond)
			r.Links.Mu.Lock()
			r.Links.AgentVersion, r.Links.BootID = release.AgentVersion, "boot-2"
			r.Links.Mu.Unlock()
		}()
	}
	r.Links.Mu.Unlock()
	id, err := r.Svc.StartUpdate(context.Background(), 1, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, id); v.State != relayops.JobOK {
		t.Fatalf("job = %+v", v)
	}
	if got := r.Links.Calls[len(r.Links.Calls)-1]; got != ctrl.MUpdateCommit {
		t.Fatalf("calls = %v: the new agent was not committed", r.Links.Calls)
	}
}

// A re-sent update finds the old process already on the target version: only a
// new process may be committed.
func TestTheOldProcessIsNeverCommitted(t *testing.T) {
	r := relaytest.NewRig(func(c *relayops.ServiceConfig) { c.UpdateWait = 700 * time.Millisecond })
	r.Installed(t)
	r.Links.Mu.Lock()
	r.Links.AgentVersion, r.Links.BootID = release.AgentVersion, "boot-1" // never restarts
	r.Links.Mu.Unlock()
	id, err := r.Svc.StartUpdate(context.Background(), 1, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, id); v.State != relayops.JobFailed {
		t.Fatalf("job = %+v", v)
	}
	if r.Links.CallsOf(ctrl.MUpdateCommit) != 0 {
		t.Fatal("committed the process that was running before the update")
	}
}

func TestAgentThatDoesNotComeBackIsNotCommitted(t *testing.T) {
	r := relaytest.NewRig(func(c *relayops.ServiceConfig) { c.UpdateWait = 700 * time.Millisecond })
	r.Installed(t)
	r.Links.Mu.Lock()
	r.Links.AgentVersion = "old" // never changes
	r.Links.Mu.Unlock()
	id, err := r.Svc.StartUpdate(context.Background(), 1, "agent")
	if err != nil {
		t.Fatal(err)
	}
	v := r.Wait(t, id)
	if v.State != relayops.JobFailed || !strings.Contains(v.Error, "restores the previous one") {
		t.Fatalf("job = %+v", v)
	}
	r.Links.Mu.Lock()
	defer r.Links.Mu.Unlock()
	for _, m := range r.Links.Calls {
		if m == ctrl.MUpdateCommit {
			t.Fatal("committed a binary that never proved itself")
		}
	}
}

// Nothing new to install: the node isn't offered the update, and a request for it is refused.
func TestAnUpdateThatChangesNothingIsRefused(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	ctx := context.Background()
	same := strings.Repeat("a", 64) // what relaytest.Bins hands out
	set := func(ver, build, telemt string) {
		r.Links.Mu.Lock()
		r.Links.AgentVersion, r.Links.AgentBuild, r.Links.TelemtVer = ver, build, telemt
		r.Links.Mu.Unlock()
	}
	set(release.AgentVersion, same, release.TelemtVersion)
	if d := r.Svc.Due(1, "amd64"); d.Agent || d.Telemt || d.AgentBuild != same {
		t.Fatalf("due = %+v", d)
	}
	for _, kind := range []string{"agent", "telemt"} {
		if _, err := r.Svc.StartUpdate(ctx, 1, kind); !errors.Is(err, relayops.ErrUpToDate) {
			t.Errorf("%s: %v", kind, err)
		}
	}
	if len(r.Links.RawReqs) != 0 {
		t.Fatalf("uploaded %+v", r.Links.RawReqs)
	}

	// another build of the same version is an update: dev builds all share one
	set(release.AgentVersion, strings.Repeat("b", 64), "3.4.24")
	if d := r.Svc.Due(1, "amd64"); !d.Agent || !d.Telemt {
		t.Fatalf("another build: due = %+v", d)
	}
	// an agent too old to name its build is always offered one
	set(release.AgentVersion, "", "v"+release.TelemtVersion)
	if d := r.Svc.Due(1, "amd64"); !d.Agent || d.Telemt {
		t.Fatalf("an old agent: due = %+v", d)
	}
	// before the first report there is nothing to compare
	set("", "", "")
	if d := r.Svc.Due(1, "amd64"); d.Agent || d.Telemt {
		t.Fatalf("no report: due = %+v", d)
	}
}

// The process that answers after an update has to be the build that was pushed.
func TestAnotherBuildAnsweringAfterAnUpdateIsNotCommitted(t *testing.T) {
	r := relaytest.NewRig(func(c *relayops.ServiceConfig) { c.UpdateWait = 700 * time.Millisecond })
	r.Installed(t)
	r.Links.Mu.Lock()
	r.Links.AgentVersion, r.Links.AgentBuild, r.Links.BootID = release.AgentVersion, strings.Repeat("b", 64), "boot-1"
	r.Links.AfterRaw = func() {
		r.Links.Mu.Lock()
		r.Links.BootID, r.Links.AgentBuild = "boot-2", strings.Repeat("c", 64) // restarted, but not into ours
		r.Links.Mu.Unlock()
	}
	r.Links.Mu.Unlock()
	id, err := r.Svc.StartUpdate(context.Background(), 1, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, id); v.State != relayops.JobFailed {
		t.Fatalf("job = %+v", v)
	}
	if r.Links.CallsOf(ctrl.MUpdateCommit) != 0 {
		t.Fatal("committed a build nobody pushed")
	}
}

func TestUpdateGuards(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	if _, err := r.Svc.StartUpdate(context.Background(), 1, "kernel"); !errors.Is(err, relayops.ErrInvalid) {
		t.Errorf("bad kind: %v", err)
	}
	row, _ := r.Store.Get(context.Background(), 1)
	row.Arch = ""
	r.Store.Save(context.Background(), row)
	if _, err := r.Svc.StartUpdate(context.Background(), 1, "agent"); !errors.Is(err, relayops.ErrUnknownArch) {
		t.Errorf("unknown arch: %v", err)
	}
	row.Arch = "amd64"
	r.Store.Save(context.Background(), row)
	r.Links.SetUp(1, false)
	if _, err := r.Svc.StartUpdate(context.Background(), 1, "agent"); !errors.Is(err, relayops.ErrNotConnected) {
		t.Errorf("link down: %v", err)
	}
}

func TestRotationSwitchesTheAgentThenTheLinkThenConfirms(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	before, _ := r.Store.Get(context.Background(), 1)
	r.Links.RedialUp = true
	id, err := r.Svc.StartRotate(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, id); v.State != relayops.JobOK {
		t.Fatalf("job = %+v", v)
	}
	after, _ := r.Store.Get(context.Background(), 1)
	if after.Token == before.Token || after.RealityPub == before.RealityPub || after.ShortID == before.ShortID || after.NextSeed != "" {
		t.Fatalf("credentials: %+v", after)
	}
	if before.KeysRotatedAt != nil || after.KeysRotatedAt == nil || time.Since(*after.KeysRotatedAt) > time.Minute {
		t.Fatalf("rotation date: before %v after %v", before.KeysRotatedAt, after.KeysRotatedAt)
	}
	// first the candidate beside the current set, then the switch
	if n := len(r.Links.CredsSet); n != 2 || r.Links.CredsSet[0].NextSeed == "" || r.Links.CredsSet[0].Token != before.Token || r.Links.CredsSet[1].Token != after.Token || r.Links.Redials != 1 {
		t.Fatalf("setCreds=%+v redials=%d", r.Links.CredsSet, r.Links.Redials)
	}
	if n := len(r.Links.Calls); n != 2 || r.Links.Calls[0] != ctrl.MKeysRotate || r.Links.Calls[1] != ctrl.MKeysRotate {
		t.Fatalf("calls = %v: want rotate, then the finishing rotate", r.Links.Calls)
	}
}

// The agent's refusal is an answer: it kept what it had, and so does the panel.
func TestARefusedRotationChangesNothing(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	before, _ := r.Store.Get(context.Background(), 1)
	r.Links.Fail = func(m string, _ int) error {
		if m == ctrl.MKeysRotate {
			return &ctrl.Error{Code: "bad_request", Msg: "rotate needs a token, a 32-byte key and short ids"}
		}
		return nil
	}
	id, _ := r.Svc.StartRotate(context.Background(), 1)
	if v := r.Wait(t, id); v.State != relayops.JobFailed {
		t.Fatalf("job = %+v", v)
	}
	after, _ := r.Store.Get(context.Background(), 1)
	if after.Token != before.Token || after.NextSeed != "" || after.KeysRotatedAt != nil {
		t.Fatal("the panel kept credentials the agent did not accept")
	}
	if last := r.Links.CredsSet[len(r.Links.CredsSet)-1]; last.Token != before.Token || last.NextSeed != "" {
		t.Fatalf("the link still dials the refused set: %+v", last)
	}
	if r.Links.CallsOf(ctrl.MKeysRotate) != 1 {
		t.Fatal("a refusal was retried")
	}
}

// A lost reply is not a refusal: the same rotation is sent again, which the
// agent takes as a no-op if the first one landed.
func TestALostRotateReplyIsSentAgain(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.RedialUp = true
	r.Links.Fail = func(m string, n int) error {
		if m == ctrl.MKeysRotate && n == 0 {
			return errors.New("stream reset") // applied, reply lost
		}
		return nil
	}
	id, _ := r.Svc.StartRotate(context.Background(), 1)
	if v := r.Wait(t, id); v.State != relayops.JobOK {
		t.Fatalf("job = %+v", v)
	}
	var sent []ctrl.KeysRotate
	for i, m := range r.Links.Calls {
		if k, ok := r.Links.Reqs[i].(ctrl.KeysRotate); ok && m == ctrl.MKeysRotate && !k.Finish {
			sent = append(sent, k)
		}
	}
	if len(sent) != 2 || !bytes.Equal(sent[0].Token, sent[1].Token) || !bytes.Equal(sent[0].PrivateKey, sent[1].PrivateKey) {
		t.Fatalf("rotations sent: %d, the same both times: %v", len(sent), len(sent) == 2 && bytes.Equal(sent[0].Token, sent[1].Token))
	}
	after, _ := r.Store.Get(context.Background(), 1)
	if after.Token != hex.EncodeToString(sent[0].Token) || after.NextSeed != "" {
		t.Fatalf("stored = %+v", after)
	}
}

// When no answer comes at all, the panel keeps both sets; the next rotation
// finishes that same one rather than starting over.
func TestAnUnansweredRotationIsFinishedByTheNextOne(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	before, _ := r.Store.Get(context.Background(), 1)
	r.Links.RedialUp = true
	r.Links.Fail = func(m string, _ int) error {
		if m == ctrl.MKeysRotate {
			return errors.New("agent did not answer")
		}
		return nil
	}
	id, _ := r.Svc.StartRotate(context.Background(), 1)
	if v := r.Wait(t, id); v.State != relayops.JobFailed || !strings.Contains(v.Error, "again") {
		t.Fatalf("job = %+v", v)
	}
	mid, _ := r.Store.Get(context.Background(), 1)
	if mid.Token != before.Token || mid.NextSeed == "" {
		t.Fatalf("the candidate was not kept beside the current set: %+v", mid)
	}
	cr, err := mid.LinkCreds()
	if err != nil || cr.Next == nil || hex.EncodeToString(cr.Token) != before.Token {
		t.Fatalf("link creds = %+v %v", cr, err)
	}
	want := cr.Next.Token

	r.Links.Mu.Lock()
	r.Links.Fail = nil
	r.Links.Mu.Unlock()
	id, err = r.Svc.StartRotate(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, id); v.State != relayops.JobOK {
		t.Fatalf("second job = %+v", v)
	}
	after, _ := r.Store.Get(context.Background(), 1)
	if after.Token != hex.EncodeToString(want) || after.NextSeed != "" {
		t.Fatalf("the second rotation did not finish the first: %+v", after)
	}
}

func TestARotationThatCannotReconnectSaysHowToRecover(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.RedialUp = false // fresh connections do not come up
	id, _ := r.Svc.StartRotate(context.Background(), 1)
	v := r.Wait(t, id)
	if v.State != relayops.JobFailed || !strings.Contains(v.Error, "repair") {
		t.Fatalf("job = %+v", v)
	}
	for _, m := range r.Links.Calls[1:] {
		if m == ctrl.MKeysRotate {
			t.Fatal("finished a rotation that was never confirmed")
		}
	}
}

// Once an attempt went unanswered the agent may have switched: a later error
// only drops the candidate when it is a definite refusal of the request itself.
func TestARotationRetriedAfterALostReplyKeepsTheCandidate(t *testing.T) {
	for code, keep := range map[string]bool{"internal": true, "uninstalling": true, "bad_request": false} {
		t.Run(code, func(t *testing.T) {
			r := relaytest.NewRig()
			r.Installed(t)
			before, _ := r.Store.Get(context.Background(), 1)
			r.Links.Fail = func(m string, n int) error {
				switch {
				case m != ctrl.MKeysRotate:
					return nil
				case n == 0:
					return errors.New("stream reset") // the agent switched, its reply was lost
				default:
					return &ctrl.Error{Code: code, Msg: "no"}
				}
			}
			id, _ := r.Svc.StartRotate(context.Background(), 1)
			if v := r.Wait(t, id); v.State != relayops.JobFailed {
				t.Fatalf("job = %+v", v)
			}
			after, _ := r.Store.Get(context.Background(), 1)
			if after.Token != before.Token {
				t.Fatal("the panel switched on an unconfirmed rotation")
			}
			if kept := after.NextSeed != ""; kept != keep {
				t.Fatalf("candidate kept = %v, want %v", kept, keep)
			}
			if last := r.Links.CredsSet[len(r.Links.CredsSet)-1]; (last.NextSeed != "") != keep {
				t.Fatalf("the link's candidate = %q, want kept %v", last.NextSeed, keep)
			}
		})
	}
	// a candidate left by an earlier job is just as unknown
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.Fail = func(m string, n int) error {
		if m == ctrl.MKeysRotate {
			if n < 3 { // the whole first job goes unanswered
				return errors.New("agent did not answer")
			}
			return &ctrl.Error{Code: "internal", Msg: "disk full"}
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		id, _ := r.Svc.StartRotate(context.Background(), 1)
		r.Wait(t, id)
		if after, _ := r.Store.Get(context.Background(), 1); after.NextSeed == "" {
			t.Fatalf("job %d dropped the candidate an earlier attempt may have installed", i+1)
		}
	}
}

// A new agent whose telemt does not come back as it was is not committed: its
// trial runs out and the previous binary returns.
func TestAnUpdatedAgentWhoseTelemtIsHeldIsNotCommitted(t *testing.T) {
	r := relaytest.NewRig(func(c *relayops.ServiceConfig) { c.UpdateWait = 700 * time.Millisecond })
	r.Installed(t)
	var updated atomic.Bool
	r.Links.Mu.Lock()
	r.Links.Respond = func(m string, resp any) {
		st, ok := resp.(*ctrl.Status)
		if !ok {
			return
		}
		if !updated.Load() {
			st.AgentVersion, st.BootID, st.Telemt.Running = "old", "boot-1", true
			return
		}
		st.AgentVersion, st.BootID = release.AgentVersion, "boot-2"
		st.Telemt.Held = "the saved state could not be applied: rules: nft: no such file or directory"
	}
	r.Links.AfterRaw = func() { updated.Store(true) }
	r.Links.Mu.Unlock()
	id, err := r.Svc.StartUpdate(context.Background(), 1, "agent")
	if err != nil {
		t.Fatal(err)
	}
	v := r.Wait(t, id)
	if v.State != relayops.JobFailed || !strings.Contains(v.Error, "telemt") {
		t.Fatalf("job = %+v", v)
	}
	if r.Links.CallsOf(ctrl.MUpdateCommit) != 0 {
		t.Fatal("committed an agent that keeps telemt stopped")
	}
}

// A box is cleaned up only when this job changed it and it had no agent
// before: the panel failing to remember the node is no reason to remove one.
func TestARegisterFailureOverAnEarlierAgentLeavesTheBoxAlone(t *testing.T) {
	for name, tweak := range map[string]func(*relaytest.Rig){
		"node":   func(r *relaytest.Rig) { r.Admin.CreateE = errors.New("database is locked") },
		"record": func(r *relaytest.Rig) { r.Store.SaveE = errors.New("database is locked") },
	} {
		r := relaytest.NewRig()
		r.SSH.WasInstalled = true
		tweak(r)
		id, _ := r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
		if v := r.Wait(t, id); v.State != relayops.JobFailed {
			t.Fatalf("%s: job = %+v", name, v)
		}
		if r.SSH.Uninstalls != 0 {
			t.Errorf("%s: the agent that was on the box was uninstalled", name)
		}
		// on a box that had none, the half-install is taken back
		r = relaytest.NewRig()
		tweak(r)
		id, _ = r.Svc.StartInstall(context.Background(), relaytest.GoodInstall())
		if r.Wait(t, id); r.SSH.Uninstalls != 1 {
			t.Errorf("%s: uninstalls = %d on a box this job set up", name, r.SSH.Uninstalls)
		}
	}
}

// A repair tells the installer the agent on the box is the node's own, so a
// reset may replace its credentials; a fresh install never says that.
func TestOnlyARepairMayReplaceAnAgent(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.RedialUp = true
	if r.SSH.Installed[0].Repair || r.SSH.Installed[0].KeepState {
		t.Fatal("a fresh install asked to work over an agent")
	}
	for _, reset := range []bool{false, true} {
		jid, err := r.Svc.StartRepair(context.Background(), 1, relayops.Login{Password: "pw"}, reset)
		if err != nil {
			t.Fatal(err)
		}
		r.Wait(t, jid)
		if last := r.SSH.Installed[len(r.SSH.Installed)-1]; !last.Repair || last.KeepState == reset {
			t.Fatalf("reset=%v: request = repair %v keep %v", reset, last.Repair, last.KeepState)
		}
	}
}

// The cover site the box runs is what the hub's SNI must say: a repair takes
// it from the box, so a lost ack does not leave the link refused for good.
func TestRepairAdoptsTheCoverSiteTheBoxRuns(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.RedialUp = true
	row, _ := r.Store.Get(context.Background(), 1)
	cr, _ := installer.NewCreds()
	r.SSH.Result = &installer.Result{AgentID: row.AgentID, Creds: cr, HubPort: 8443, CapturePort: 9443, Domain: "moved.example.com", Started: true}
	jid, _ := r.Svc.StartRepair(context.Background(), 1, relayops.Login{Password: "pw"}, false)
	if v := r.Wait(t, jid); v.State != relayops.JobOK {
		t.Fatalf("repair = %+v", v)
	}
	after, _ := r.Store.Get(context.Background(), 1)
	if after.Disguise != "moved.example.com" {
		t.Fatalf("record disguise = %q", after.Disguise)
	}
	if last := r.Links.CredsSet[len(r.Links.CredsSet)-1]; last.Disguise != "moved.example.com" {
		t.Fatalf("the link still dials %q", last.Disguise)
	}
	// a result that does not say keeps the record's
	r.SSH.Result.Domain = ""
	jid, _ = r.Svc.StartRepair(context.Background(), 1, relayops.Login{Password: "pw"}, false)
	r.Wait(t, jid)
	if after, _ := r.Store.Get(context.Background(), 1); after.Disguise != "moved.example.com" {
		t.Fatalf("an empty domain replaced the record's: %q", after.Disguise)
	}
}

// Only a repair that finds another cover site on the box tells the links; a store keeps
// its own cover column over every stale record, so this is how the adopted one gets there.
func TestOnlyARepairThatMovesTheCoverSiteAdoptsIt(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.RedialUp = true
	ctx := context.Background()
	row, _ := r.Store.Get(ctx, 1)
	cr, _ := installer.NewCreds()
	r.SSH.Result = &installer.Result{AgentID: row.AgentID, Creds: cr, HubPort: 8443, CapturePort: 9443, Domain: row.Disguise, Started: true}
	repair := func() {
		t.Helper()
		jid, err := r.Svc.StartRepair(ctx, 1, relayops.Login{Password: "pw"}, false)
		if err != nil {
			t.Fatal(err)
		}
		if v := r.Wait(t, jid); v.State != relayops.JobOK {
			t.Fatalf("repair = %+v", v)
		}
	}
	repair() // the box runs the site the panel knows
	if got := r.Links.Adopted(); len(got) != 0 {
		t.Fatalf("an unchanged cover site was adopted: %+v", got)
	}
	r.SSH.Result.Domain = "moved.example.com"
	repair()
	want := []relaytest.Adopt{{NodeID: 1, Domain: "moved.example.com"}}
	if got := r.Links.Adopted(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("adopted = %+v, want %+v", got, want)
	}
	for _, start := range []func() (string, error){
		func() (string, error) { return r.Svc.StartRotate(ctx, 1) },
		func() (string, error) { return r.Svc.StartUpdate(ctx, 1, "telemt") },
	} {
		jid, err := start()
		if err != nil {
			t.Fatal(err)
		}
		r.Wait(t, jid)
	}
	if got := r.Links.Adopted(); len(got) != 1 {
		t.Fatalf("a rotation or update adopted a cover site: %+v", got)
	}
}

// The adopted name must be in place before the link redials on it.
func TestARepairFailsWhenTheLinksCannotAdoptTheCoverSite(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.RedialUp = true
	row, _ := r.Store.Get(context.Background(), 1)
	cr, _ := installer.NewCreds()
	r.SSH.Result = &installer.Result{AgentID: row.AgentID, Creds: cr, HubPort: 8443, CapturePort: 9443, Domain: "moved.example.com", Started: true}
	r.Links.Mu.Lock()
	r.Links.AdoptErr = errors.New("database is locked")
	redials := r.Links.Redials
	r.Links.Mu.Unlock()
	jid, _ := r.Svc.StartRepair(context.Background(), 1, relayops.Login{Password: "pw"}, false)
	if v := r.Wait(t, jid); v.State != relayops.JobFailed || !strings.Contains(v.Error, "database is locked") {
		t.Fatalf("repair = %+v", v)
	}
	r.Links.Mu.Lock()
	defer r.Links.Mu.Unlock()
	if r.Links.Redials != redials {
		t.Fatal("the link redialled before it had the adopted cover site")
	}
}

func linkPortReqs(l *relaytest.Links) []ctrl.LinkPort {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	var out []ctrl.LinkPort
	for _, r := range l.Reqs {
		if lp, ok := r.(ctrl.LinkPort); ok {
			out = append(out, lp)
		}
	}
	return out
}

func TestTheLinkMovesToANewPortOnceThePanelGetsIn(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.Mu.Lock()
	r.Links.RedialUp = true
	r.Links.Mu.Unlock()
	from := func() int { row, _ := r.Store.Get(context.Background(), 1); return row.HubPort }()
	id, err := r.Svc.StartLinkPort(context.Background(), 1, 9000)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, id); v.State != relayops.JobOK {
		t.Fatalf("job = %+v", v)
	}
	if row, _ := r.Store.Get(context.Background(), 1); row.HubPort != 9000 {
		t.Fatalf("saved port %d, was %d", row.HubPort, from)
	}
	reqs := linkPortReqs(r.Links)
	if len(reqs) != 2 || reqs[0].Port != 9000 || !reqs[1].Finish {
		t.Fatalf("asked the agent %+v", reqs)
	}
}

// A port the panel can't reach (a provider firewall) moves the panel back, and the agent closes it again.
func TestAnUnreachablePortLeavesTheLinkWhereItWas(t *testing.T) {
	r := relaytest.NewRig(func(c *relayops.ServiceConfig) { c.LinkWait = 700 * time.Millisecond })
	r.Installed(t)
	from := func() int { row, _ := r.Store.Get(context.Background(), 1); return row.HubPort }()
	r.Links.Mu.Lock()
	r.Links.OnRedial = func(n int) bool { return n > 1 } // the new port never answers; the old one does
	r.Links.Mu.Unlock()
	id, err := r.Svc.StartLinkPort(context.Background(), 1, 9000)
	if err != nil {
		t.Fatal(err)
	}
	v := r.Wait(t, id)
	if v.State != relayops.JobFailed || !strings.Contains(v.Error, "could not reach the node on port 9000") {
		t.Fatalf("job = %+v", v)
	}
	if row, _ := r.Store.Get(context.Background(), 1); row.HubPort != from {
		t.Fatalf("the panel stayed on %d, want %d", row.HubPort, from)
	}
	reqs := linkPortReqs(r.Links)
	if len(reqs) != 2 || reqs[0].Port != 9000 || !reqs[1].Abort {
		t.Fatalf("asked the agent %+v", reqs)
	}
}

func TestAPortTheAgentRefusesChangesNothing(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	from := func() int { row, _ := r.Store.Get(context.Background(), 1); return row.HubPort }()
	r.Links.Mu.Lock()
	r.Links.Fail = func(m string, n int) error {
		if m == ctrl.MLinkPort {
			return &ctrl.Error{Code: "port_busy", Msg: "port 9000 can't be opened on the node: address already in use"}
		}
		return nil
	}
	r.Links.Mu.Unlock()
	id, _ := r.Svc.StartLinkPort(context.Background(), 1, 9000)
	v := r.Wait(t, id)
	if v.State != relayops.JobFailed || !strings.Contains(v.Error, "already in use") {
		t.Fatalf("job = %+v", v)
	}
	if row, _ := r.Store.Get(context.Background(), 1); row.HubPort != from || r.Links.Redials != 0 {
		t.Fatalf("port %d, redials %d", row.HubPort, r.Links.Redials)
	}
}

func TestLinkPortGuards(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	row, _ := r.Store.Get(context.Background(), 1)
	for _, p := range []int{0, 70000, row.HubPort, row.CapturePort, row.SSHPort} {
		if _, err := r.Svc.StartLinkPort(context.Background(), 1, p); !errors.Is(err, relayops.ErrInvalid) {
			t.Errorf("port %d: %v", p, err)
		}
	}
	r.Links.SetUp(1, false)
	if _, err := r.Svc.StartLinkPort(context.Background(), 1, 9000); !errors.Is(err, relayops.ErrNotConnected) {
		t.Errorf("link down: %v", err)
	}
}

// A node that answers at its new address keeps it; the job names no other step.
func TestTheAddressMovesOnceTheLinkComesUpThere(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.Mu.Lock()
	r.Links.RedialUp = true
	r.Links.Mu.Unlock()
	id, err := r.Svc.StartAddress(context.Background(), 1, " vps.example.com ")
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, id); v.State != relayops.JobOK {
		t.Fatalf("job = %+v", v)
	}
	if h, _ := r.Admin.Host(context.Background(), 1); h != "vps.example.com" {
		t.Fatalf("host %q", h)
	}
}

// Nothing answering at the new address puts the old one back.
func TestAnAddressNothingAnswersAtKeepsTheOldOne(t *testing.T) {
	r := relaytest.NewRig(func(c *relayops.ServiceConfig) { c.LinkWait = 700 * time.Millisecond })
	r.Installed(t)
	from, _ := r.Admin.Host(context.Background(), 1)
	r.Links.Mu.Lock()
	r.Links.OnRedial = func(n int) bool { return n > 1 } // the new address never answers; the old one does
	r.Links.Mu.Unlock()
	id, err := r.Svc.StartAddress(context.Background(), 1, "198.51.100.9")
	if err != nil {
		t.Fatal(err)
	}
	v := r.Wait(t, id)
	if v.State != relayops.JobFailed || !strings.Contains(v.Error, "nothing answered at 198.51.100.9") {
		t.Fatalf("job = %+v", v)
	}
	if h, _ := r.Admin.Host(context.Background(), 1); h != from {
		t.Fatalf("host %q, want %q back", h, from)
	}
}

func TestAddressesTheServiceRefuses(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	from, _ := r.Admin.Host(context.Background(), 1)
	for _, h := range []string{"", "bad host", from} {
		if _, err := r.Svc.StartAddress(context.Background(), 1, h); err == nil {
			t.Errorf("%q: want a refusal", h)
		}
	}
}

// fetching the builds can take a while on a slow router: it shows as a step, not a stall
func TestRepairAndUpdateShowTheirDownload(t *testing.T) {
	r := relaytest.NewRig()
	r.Installed(t)
	r.Links.RedialUp = true
	jid, err := r.Svc.StartRepair(context.Background(), 1, relayops.Login{Password: "pw"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, jid); v.State != relayops.JobOK || v.Steps[0].Name != "download" || v.Steps[0].State != "ok" {
		t.Fatalf("repair = %+v", v)
	}
	id, err := r.Svc.StartUpdate(context.Background(), 1, "telemt")
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, id); v.State != relayops.JobOK || v.Steps[0].Name != "download" || v.Steps[0].State != "ok" || v.Steps[1].Name != "upload" {
		t.Fatalf("update = %+v", v)
	}
}
