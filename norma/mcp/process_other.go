//go:build !linux

package mcp

import "os/exec"

func configureStdioProcess(cmd *exec.Cmd) {}
func killStdioProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
