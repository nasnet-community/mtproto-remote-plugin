package relayops

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
)

// updateWait is the default for how long an updated agent has to come back
// before we give up waiting. It rolls itself back a few minutes later if nobody
// commits it.
const updateWait = 3 * time.Minute

var (
	ErrUpToDate     = errors.New("relay: the node already runs what this panel would install")
	ErrNotConnected = errors.New("relay: the node's link is down")
	ErrUnknownArch  = errors.New("relay: this node's CPU architecture is unknown: run a repair once to record it")
)

func (s *Service) linkUp(nodeID uint) bool {
	v, ok := s.cfg.Links.View(nodeID)
	return ok && v.Up
}

// Due is which updates a node can take from this panel.
type Due struct {
	Agent, Telemt bool
	// AgentBuild is the build this panel installs on the node ("" when it has none).
	AgentBuild string
}

// Due compares what the node last reported with what an update would install.
// The agent goes by build, since dev builds share a version; one too old to report
// its build is always offered, as is a proxy whose version the agent can't read.
func (s *Service) Due(nodeID uint, arch string) Due {
	v, _ := s.cfg.Links.View(nodeID)
	return s.DueFor(v, arch)
}

// DueFor is Due for a report the caller holds already.
func (s *Service) DueFor(v LinkView, arch string) Due {
	var d Due
	if arch != "" && s.cfg.Bins != nil {
		d.AgentBuild = s.cfg.Bins.AgentBuild(release.Arch(arch))
	}
	if v.AgentVersion == "" { // no report yet: nothing to compare
		return d
	}
	d.Agent = d.AgentBuild != "" && (v.AgentBuild == "" || !strings.EqualFold(v.AgentBuild, d.AgentBuild))
	d.Telemt = v.TelemtVersion == "" || strings.TrimPrefix(v.TelemtVersion, "v") != release.TelemtVersion
	return d
}

// upToDate is only the sure case: a node that said it runs exactly this.
func (s *Service) upToDate(nodeID uint, arch, kind string) bool {
	v, ok := s.cfg.Links.View(nodeID)
	if !ok {
		return false
	}
	if kind == "telemt" {
		return v.TelemtVersion != "" && strings.TrimPrefix(v.TelemtVersion, "v") == release.TelemtVersion
	}
	if s.cfg.Bins == nil {
		return false
	}
	build := s.cfg.Bins.AgentBuild(release.Arch(arch))
	return build != "" && strings.EqualFold(v.AgentBuild, build)
}

// StartUpdate pushes the pinned agent or telemt binary over the link. kind is
// "agent" or "telemt". A node already on it is refused with ErrUpToDate.
func (s *Service) StartUpdate(ctx context.Context, nodeID uint, kind string) (string, error) {
	if kind != "agent" && kind != "telemt" {
		return "", invalid("kind must be agent or telemt")
	}
	row, err := s.cfg.Store.Get(ctx, nodeID)
	if err != nil {
		return "", err
	}
	if row.Arch == "" {
		return "", ErrUnknownArch
	}
	if !s.linkUp(nodeID) {
		return "", ErrNotConnected
	}
	if s.upToDate(nodeID, row.Arch, kind) {
		return "", ErrUpToDate
	}
	steps := []string{"download", "upload"}
	if kind == "agent" {
		steps = append(steps, "restart", "commit")
	}
	return s.start("update-"+kind, []string{nodeKey(nodeID)}, func(ctx context.Context, j *job) error {
		return s.runUpdate(ctx, j, row, kind)
	}, planned(steps...))
}

func (s *Service) runUpdate(ctx context.Context, j *job, row *Record, kind string) error {
	arch := release.Arch(row.Arch)
	var bin installer.Bin
	var ver string
	var err error
	j.step(installer.Step{Name: "download", State: "running"})
	if kind == "agent" {
		bin, err = s.cfg.Bins.Agent(ctx, arch)
		ver = release.AgentVersion
	} else {
		bin, err = s.cfg.Bins.Telemt(ctx, arch)
		ver = release.TelemtVersion
	}
	if err != nil {
		j.step(installer.Step{Name: "download", State: "failed", Detail: err.Error()})
		return err
	}
	j.step(installer.Step{Name: "download", State: "ok"})
	j.setNode(row.NodeID)
	// which process answers now: only a different one may be committed later
	var before ctrl.Status
	if kind == "agent" {
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := s.cfg.Links.Call(sctx, row.NodeID, ctrl.MStatus, nil, &before)
		cancel()
		if err != nil {
			j.step(installer.Step{Name: "upload", State: "failed", Detail: err.Error()})
			return fmt.Errorf("relay: could not read the agent's status before the update: %w", err)
		}
	}

	j.step(installer.Step{Name: "upload", State: "running"})
	if bin.Open == nil {
		err = errors.New("relay: no binary to upload")
		j.step(installer.Step{Name: "upload", State: "failed", Detail: err.Error()})
		return err
	}
	rc, err := bin.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	req := ctrl.UpdateRequest{Kind: kind, Arch: row.Arch, Size: bin.Size, SHA256: bin.SHA256, Ver: ver}
	cctx, cancel := context.WithTimeout(ctx, uploadWait(bin.Size))
	defer cancel()
	var out map[string]any
	if err := s.cfg.Links.CallRaw(cctx, row.NodeID, ctrl.MUpdate, req, rc, &out); err != nil {
		j.step(installer.Step{Name: "upload", State: "failed", Detail: err.Error()})
		return err
	}
	j.step(installer.Step{Name: "upload", State: "ok"})
	if kind == "telemt" {
		return nil // the agent restarted telemt and checked it before answering
	}

	// a new process on the new version, then commit it (the old one may report that version already)
	j.step(installer.Step{Name: "restart", State: "running"})
	var held string // why the new process keeps telemt down, if it came back at all
	fresh := func() bool {
		var now ctrl.Status
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if s.cfg.Links.Call(sctx, row.NodeID, ctrl.MStatus, nil, &now) != nil || now.AgentVersion != ver {
			return false
		}
		if now.AgentBuild != "" && !strings.EqualFold(now.AgentBuild, bin.SHA256) {
			return false // some other build answers
		}
		if before.BootID != "" || now.BootID != "" {
			if now.BootID == before.BootID {
				return false
			}
		} else if before.AgentVersion == ver {
			return false // agents too old to say which process they are
		}
		// telemt must be as well off as before: a new agent that cannot run it is not kept
		if before.Telemt.Held == "" && now.Telemt.Held != "" {
			held = now.Telemt.Held
			return false
		}
		if before.Telemt.Running && !now.Telemt.Running {
			held = "telemt is not running"
			return false
		}
		return true
	}
	if s.poll(ctx, s.cfg.UpdateWait, fresh) {
		j.step(installer.Step{Name: "restart", State: "ok"})
		ccx, cc := context.WithTimeout(ctx, 20*time.Second)
		defer cc()
		if err := s.cfg.Links.Call(ccx, row.NodeID, ctrl.MUpdateCommit, nil, nil); err != nil {
			j.step(installer.Step{Name: "commit", State: "failed", Detail: err.Error()})
			return err
		}
		j.step(installer.Step{Name: "commit", State: "ok"})
		return nil
	}
	err = errors.New("the new agent did not come back; it restores the previous one by itself within a few minutes")
	if held != "" {
		err = fmt.Errorf("the new agent came back but keeps telemt stopped (%s); it restores the previous one by itself within a few minutes", held)
	}
	j.step(installer.Step{Name: "restart", State: "failed", Detail: err.Error()})
	return err
}

// uploadWait gives an upload over the link at least 5 minutes, more for a big file
// on a slow line: 15 MB of proxy at 400 kbit/s needs about 5 minutes on its own.
func uploadWait(size int64) time.Duration {
	return max(5*time.Minute, time.Duration(size/20_000+1)*time.Second)
}

// StartRotate: remember the new credentials, the agent switches, the panel follows, a new connection confirms.
func (s *Service) StartRotate(ctx context.Context, nodeID uint) (string, error) {
	row, err := s.cfg.Store.Get(ctx, nodeID)
	if err != nil {
		return "", err
	}
	if !s.linkUp(nodeID) {
		return "", ErrNotConnected
	}
	return s.start("rotate", []string{nodeKey(nodeID)}, func(ctx context.Context, j *job) error {
		return s.runRotate(ctx, j, row)
	}, planned("rotate", "verify"))
}

func (s *Service) runRotate(ctx context.Context, j *job, row *Record) error {
	j.setNode(row.NodeID)
	j.step(installer.Step{Name: "rotate", State: "running"})
	fail := func(err error, detail string) error {
		j.step(installer.Step{Name: "rotate", State: "failed", Detail: detail})
		return err
	}
	// 1. remember the candidate first; an unanswered earlier one is finished, not replaced
	unknown := row.NextSeed != "" // that earlier one may have landed
	if row.NextSeed == "" {
		seed, err := newSeed()
		if err != nil {
			return err
		}
		next := *row
		next.NextSeed = seed
		if err := s.cfg.Store.Save(ctx, &next); err != nil {
			return fail(err, "the panel could not save the new credentials; nothing changed")
		}
		row = &next
	}
	cand, err := seedCreds(row.NextSeed)
	if err != nil {
		return err
	}
	// the running link may dial either set from here on
	if err := s.cfg.Links.SetCreds(ctx, row); err != nil {
		s.cfg.Log.Warn("relay: could not hand the candidate credentials to the link", "node", row.NodeID, "err", err)
	}

	// 2. the agent switches; a lost reply is sent again, a repeat changes nothing
	req := ctrl.KeysRotate{PrivateKey: cand.Priv, ShortIDs: [][]byte{cand.ShortID[:]}, Token: cand.Token}
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && !s.linkComesUp(ctx, row.NodeID, s.cfg.LinkWait) {
			break
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = s.cfg.Links.Call(cctx, row.NodeID, ctrl.MKeysRotate, req, nil)
		cancel()
		var ce *ctrl.Error
		if err == nil || errors.As(err, &ce) {
			break
		}
		unknown = true
	}
	var refused *ctrl.Error
	switch {
	case errors.As(err, &refused) && unknown && refused.Code != "bad_request":
		// an earlier try may have switched the agent; only a refusal of the request itself rules that out
		err = fmt.Errorf("the agent answered %v after an earlier try went unanswered; the panel keeps both sets of credentials: start the rotation again to finish it", err)
		return fail(err, err.Error())
	case errors.As(err, &refused):
		// an answer: the agent kept what it had, so the panel drops the candidate
		back := *row
		back.NextSeed = ""
		if serr := s.cfg.Store.Save(ctx, &back); serr == nil {
			_ = s.cfg.Links.SetCreds(ctx, &back)
		}
		return fail(err, err.Error())
	case err != nil:
		err = fmt.Errorf("the agent did not answer (%v); the panel keeps both sets of credentials: start the rotation again to finish it", err)
		return fail(err, err.Error())
	}

	// 3. the panel follows
	updated := *row
	updated.RealityPub = hex.EncodeToString(cand.Pub)
	updated.ShortID = hex.EncodeToString(cand.ShortID[:])
	updated.Token = hex.EncodeToString(cand.Token)
	updated.NextSeed = ""
	now := time.Now().UTC()
	updated.KeysRotatedAt = &now
	if err := s.cfg.Store.Save(ctx, &updated); err != nil {
		err = fmt.Errorf("the agent switched but the panel could not save the new credentials (%v); it keeps both sets: start the rotation again to finish it", err)
		return fail(err, err.Error())
	}
	if err := s.cfg.Links.SetCreds(ctx, &updated); err != nil {
		return err
	}
	j.step(installer.Step{Name: "rotate", State: "ok"})

	// 4. prove it: fresh connections must authenticate with the new credentials
	j.step(installer.Step{Name: "verify", State: "running"})
	if err := s.cfg.Links.Redial(ctx, row.NodeID); err != nil {
		return err
	}
	// a link that still looks up may be the one just cut: Finish is safe to repeat
	for attempt := 0; attempt < 3 && s.linkComesUp(ctx, row.NodeID, s.cfg.LinkWait); attempt++ {
		fctx, fc := context.WithTimeout(ctx, 20*time.Second)
		err = s.cfg.Links.Call(fctx, row.NodeID, ctrl.MKeysRotate, ctrl.KeysRotate{Finish: true}, nil)
		fc()
		var ce *ctrl.Error
		if errors.As(err, &ce) {
			return err
		}
		if err == nil {
			j.step(installer.Step{Name: "verify", State: "ok"})
			return nil
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
	}
	err = fmt.Errorf("no connection came up with the new credentials: run a repair to re-adopt what is on the VPS")
	j.step(installer.Step{Name: "verify", State: "failed", Detail: err.Error()})
	return err
}

// StartAddress points the panel at a node's new address, say after its server
// moved. The link must come up there with the keys the node already has; if it
// doesn't, the old address comes back. Users' links follow from the address.
func (s *Service) StartAddress(ctx context.Context, nodeID uint, host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" || len(host) > 253 || (!validHost(host) && net.ParseIP(host) == nil) {
		return "", invalid("use an IP address or a host name")
	}
	if _, err := s.cfg.Store.Get(ctx, nodeID); err != nil {
		return "", err
	}
	from, err := s.cfg.Admin.Host(ctx, nodeID)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(from, host) {
		return "", invalid("the node already uses %s", host)
	}
	return s.start("address", []string{nodeKey(nodeID)}, func(ctx context.Context, j *job) error {
		return s.runAddress(ctx, j, nodeID, from, host)
	}, planned("reach"))
}

func (s *Service) runAddress(ctx context.Context, j *job, nodeID uint, from, to string) error {
	j.setNode(nodeID)
	j.step(installer.Step{Name: "reach", State: "running"})
	back := func(why error) error {
		if err := s.cfg.Admin.SetHost(ctx, nodeID, from); err != nil {
			why = fmt.Errorf("%w; the panel could not go back to %s either (%v)", why, from, err)
		}
		_ = s.cfg.Links.Redial(ctx, nodeID)
		j.step(installer.Step{Name: "reach", State: "failed", Detail: why.Error()})
		return why
	}
	if err := s.cfg.Admin.SetHost(ctx, nodeID, to); err != nil {
		err = fmt.Errorf("the panel could not save the new address (%v); the node stays on %s", err, from)
		j.step(installer.Step{Name: "reach", State: "failed", Detail: err.Error()})
		return err
	}
	if err := s.cfg.Links.Redial(ctx, nodeID); err != nil {
		return back(fmt.Errorf("the panel could not reconnect (%v), so the node stays on %s", err, from))
	}
	if !s.linkComesUp(ctx, nodeID, s.cfg.LinkWait) {
		return back(fmt.Errorf("nothing answered at %s as this node, so it stays on %s", to, from))
	}
	j.step(installer.Step{Name: "reach", State: "ok"})
	return nil
}

// StartLinkPort moves the link to another port on the node. The agent listens on
// both while the panel tries the new one; only a connection there closes the old,
// and one that never comes up moves the panel back.
func (s *Service) StartLinkPort(ctx context.Context, nodeID uint, port int) (string, error) {
	row, err := s.cfg.Store.Get(ctx, nodeID)
	if err != nil {
		return "", err
	}
	switch {
	case port < 1 || port > 65535:
		return "", invalid("the link port must be between 1 and 65535")
	case port == row.HubPort:
		return "", invalid("the link already uses port %d", port)
	case port == row.CapturePort:
		return "", invalid("port %d is the agent's capture port", port)
	case port == row.SSHPort:
		return "", invalid("port %d is the node's SSH port", port)
	}
	if !s.linkUp(nodeID) {
		return "", ErrNotConnected
	}
	return s.start("link-port", []string{nodeKey(nodeID)}, func(ctx context.Context, j *job) error {
		return s.runLinkPort(ctx, j, nodeID, row.HubPort, port)
	}, planned("open", "switch", "close"))
}

func (s *Service) runLinkPort(ctx context.Context, j *job, nodeID uint, from, to int) error {
	j.setNode(nodeID)
	call := func(req ctrl.LinkPort) error {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return s.cfg.Links.Call(cctx, nodeID, ctrl.MLinkPort, req, nil)
	}
	// the row as it is now, on a port: a settings change during the job must survive
	savePort := func(port int) error {
		row, err := s.cfg.Store.Get(ctx, nodeID)
		if err != nil {
			return err
		}
		row.HubPort = port
		return s.cfg.Store.Save(ctx, row)
	}

	// 1. the agent opens the new port beside the old one
	j.step(installer.Step{Name: "open", State: "running"})
	if err := call(ctrl.LinkPort{Port: to}); err != nil {
		j.step(installer.Step{Name: "open", State: "failed", Detail: err.Error()})
		return err
	}
	j.step(installer.Step{Name: "open", State: "ok"})

	// 2. the panel follows; the old port is still there to come back to
	j.step(installer.Step{Name: "switch", State: "running"})
	back := func(why error) error {
		if err := savePort(from); err != nil {
			return fmt.Errorf("%w; the panel could not go back to port %d either (%v): run a repair", why, from, err)
		}
		_ = s.cfg.Links.Redial(ctx, nodeID)
		if s.linkComesUp(ctx, nodeID, s.cfg.LinkWait) {
			_ = call(ctrl.LinkPort{Abort: true}) // a failed abort only leaves the new port open too
		}
		j.step(installer.Step{Name: "switch", State: "failed", Detail: why.Error()})
		return why
	}
	if err := savePort(to); err != nil {
		_ = call(ctrl.LinkPort{Abort: true})
		err = fmt.Errorf("the panel could not save the new port (%v); the node stays on %d", err, from)
		j.step(installer.Step{Name: "switch", State: "failed", Detail: err.Error()})
		return err
	}
	if err := s.cfg.Links.Redial(ctx, nodeID); err != nil {
		return back(fmt.Errorf("the panel could not reconnect (%v), so the node stays on port %d", err, from))
	}
	if !s.linkComesUp(ctx, nodeID, s.cfg.LinkWait) {
		return back(fmt.Errorf("the panel could not reach the node on port %d, so it stays on %d: a firewall in front of the VPS may block that port", to, from))
	}
	j.step(installer.Step{Name: "switch", State: "ok"})

	// 3. over a connection to the new port, the agent closes the old one
	j.step(installer.Step{Name: "close", State: "running"})
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && !s.linkComesUp(ctx, nodeID, s.cfg.LinkWait) {
			break
		}
		err = call(ctrl.LinkPort{Finish: true})
		var ce *ctrl.Error
		if err == nil || errors.As(err, &ce) {
			break
		}
	}
	if err != nil {
		err = fmt.Errorf("the node answers on port %d but still listens on %d too (%v): change the port again to finish", to, from, err)
		j.step(installer.Step{Name: "close", State: "failed", Detail: err.Error()})
		return err
	}
	j.step(installer.Step{Name: "close", State: "ok"})
	return nil
}
