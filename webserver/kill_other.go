//go:build !linux

package main

import "os/exec"

func setSysProcAttr(_ *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

// killOrphanChromes is a no-op on non-Linux platforms.
func killOrphanChromes() {}
