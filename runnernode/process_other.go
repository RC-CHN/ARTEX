//go:build !linux

package runnernode

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
