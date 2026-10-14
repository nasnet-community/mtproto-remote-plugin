//go:build linux

package supervise

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// setAttrs drops telemt to its own uid (the nft capture matches on it), keeps the
// bind-low-port capability, and ties its life to the agent's.
func setAttrs(cmd *exec.Cmd, uid, gid uint32) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM, Setpgid: true}
	if uid != 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid}
		cmd.SysProcAttr.AmbientCaps = []uintptr{unix.CAP_NET_BIND_SERVICE}
	}
}
