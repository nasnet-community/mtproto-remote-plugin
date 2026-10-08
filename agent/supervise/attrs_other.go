//go:build !linux

package supervise

import "os/exec"

func setAttrs(*exec.Cmd, uint32, uint32) {}
