//go:build !windows

package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func killListenersOnPort(port int) {
	out, err := exec.Command("lsof", "-ti", "tcp:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		return
	}
	seen := make(map[int]bool)
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
