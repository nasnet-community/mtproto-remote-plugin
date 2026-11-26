package relayops

import (
	"context"
	"net"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
)

// NewSSH is the real installer.
func NewSSH() SSH { return sshInstaller{} }

// NewSSHVia is the real installer, reaching the VPSes through dial wherever a
// target names no way of its own.
func NewSSHVia(dial func(ctx context.Context, network, addr string) (net.Conn, error)) SSH {
	return sshInstaller{dial: dial}
}

type sshInstaller struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (s sshInstaller) target(t installer.Target) installer.Target {
	if t.Dial == nil {
		t.Dial = s.dial
	}
	return t
}

func (s sshInstaller) Fingerprint(ctx context.Context, t installer.Target) (string, error) {
	return installer.Probe(ctx, s.target(t))
}

func (s sshInstaller) Arch(ctx context.Context, t installer.Target) (release.Arch, error) {
	c, err := installer.Connect(ctx, s.target(t))
	if err != nil {
		return "", err
	}
	defer c.Close()
	f, err := installer.Inspect(ctx, c)
	if err != nil {
		return "", err
	}
	return f.Arch, nil
}

func (s sshInstaller) Install(ctx context.Context, t installer.Target, req installer.Request, p installer.Progress) (*installer.Result, error) {
	c, err := installer.Connect(ctx, s.target(t))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return installer.Install(ctx, c, req, p)
}

func (s sshInstaller) Uninstall(ctx context.Context, t installer.Target, p installer.Progress) error {
	c, err := installer.Connect(ctx, s.target(t))
	if err != nil {
		return err
	}
	defer c.Close()
	return installer.Uninstall(ctx, c, p)
}

func (s sshInstaller) CheckCover(ctx context.Context, t installer.Target, domain string) (ctrl.DisguiseResult, bool, error) {
	c, err := installer.Connect(ctx, s.target(t))
	if err != nil {
		return ctrl.DisguiseResult{}, false, err
	}
	defer c.Close()
	res, checked := installer.CheckCover(ctx, c, domain)
	return res, checked, nil
}
