package relayops

import (
	"context"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
)

// NewSSH is the real installer.
func NewSSH() SSH { return sshInstaller{} }

type sshInstaller struct{}

func (sshInstaller) Fingerprint(ctx context.Context, t installer.Target) (string, error) {
	return installer.Probe(ctx, t)
}

func (sshInstaller) Arch(ctx context.Context, t installer.Target) (release.Arch, error) {
	c, err := installer.Connect(ctx, t)
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

func (sshInstaller) Install(ctx context.Context, t installer.Target, req installer.Request, p installer.Progress) (*installer.Result, error) {
	c, err := installer.Connect(ctx, t)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return installer.Install(ctx, c, req, p)
}

func (sshInstaller) Uninstall(ctx context.Context, t installer.Target, p installer.Progress) error {
	c, err := installer.Connect(ctx, t)
	if err != nil {
		return err
	}
	defer c.Close()
	return installer.Uninstall(ctx, c, p)
}

func (sshInstaller) CheckCover(ctx context.Context, t installer.Target, domain string) (ctrl.DisguiseResult, bool, error) {
	c, err := installer.Connect(ctx, t)
	if err != nil {
		return ctrl.DisguiseResult{}, false, err
	}
	defer c.Close()
	res, checked := installer.CheckCover(ctx, c, domain)
	return res, checked, nil
}
