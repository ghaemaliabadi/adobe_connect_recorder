//go:build linux

package main

import (
	"log"
	"os/exec"
	"syscall"
)

// setSysProcAttr puts the child process into its own process group so that
// killing -PGID kills the recorder, Chrome, FFmpeg, and Xvfb all at once.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGKILL to the entire process group of cmd,
// ensuring all child processes (Chrome, FFmpeg, Xvfb) are also terminated.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Negative PID means "kill the process group".
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	// Fallback: kill the process itself in case it's not the group leader.
	_ = cmd.Process.Kill()
}

// killOrphanChromes terminates any leftover Chrome/Chromium processes that
// are not part of an active recorder process group. Called periodically when
// no recording jobs are active to clean up after crashes or bugs.
func killOrphanChromes() {
	patterns := []string{"google-chrome", "chromium", "chrome"}
	for _, pat := range patterns {
		out, err := exec.Command("pgrep", "-f", pat).Output()
		if err != nil || len(out) == 0 {
			continue
		}
		if err2 := exec.Command("pkill", "-KILL", "-f", pat).Run(); err2 == nil {
			log.Printf("orphan-cleanup: killed leftover %s processes", pat)
		}
	}
}
