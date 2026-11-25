//go:build !unix

package runner

import "os/exec"

func killProcessGroupOnCancel(cmd *exec.Cmd) {}
