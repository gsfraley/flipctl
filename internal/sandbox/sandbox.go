// Package sandbox wraps plugin command argv vectors so they run with only the
// host access their manifest declares.
package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"time"
)

// Policy is the access granted to one command invocation.
type Policy struct {
	// Network shares the host network namespace. When false the command runs
	// with only a loopback interface.
	Network bool
	// Devices are /dev nodes bound into the sandbox in addition to the
	// minimal /dev (null, zero, full, random, urandom, tty).
	Devices []string
	// ReadOnly are host paths (typically command roots) bound read-only at
	// the same location.
	ReadOnly []string
	// WorkDir is the absolute working directory inside the sandbox.
	WorkDir string
}

// Sandbox turns an argv into one that runs under Policy.
type Sandbox interface {
	Name() string
	Wrap(argv []string, p Policy) ([]string, error)
}

// None runs commands directly. It exists so the daemon still works where no
// sandbox is available, and it says so loudly every time it is used.
type None struct{ Reason string }

func (None) Name() string { return "none" }

func (n None) Wrap(argv []string, p Policy) ([]string, error) {
	slog.Warn("running plugin command WITHOUT a sandbox: declared capabilities are not enforced",
		"argv0", firstArg(argv), "reason", n.Reason, "network", p.Network, "devices", p.Devices)
	return argv, nil
}

// Bwrap runs commands under bubblewrap with every namespace unshared, an
// empty root, read-only system directories, a minimal /dev and a private /tmp.
type Bwrap struct{ Path string }

func (Bwrap) Name() string { return "bwrap" }

var systemDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32", "/etc/alternatives", "/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d"}
var networkFiles = []string{"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/services", "/etc/protocols", "/etc/ssl", "/etc/ca-certificates", "/etc/pki"}

func (b Bwrap) Wrap(argv []string, p Policy) ([]string, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty argv")
	}
	out := []string{b.Path, "--die-with-parent", "--new-session", "--unshare-all"}
	if p.Network {
		out = append(out, "--share-net")
	}
	out = append(out, "--clearenv",
		"--setenv", "PATH", "/usr/local/bin:/usr/bin:/bin",
		"--setenv", "HOME", "/tmp", "--setenv", "LANG", "C", "--setenv", "LC_ALL", "C")
	for _, d := range systemDirs {
		out = append(out, "--ro-bind-try", d, d)
	}
	if p.Network {
		for _, f := range networkFiles {
			out = append(out, "--ro-bind-try", f, f)
		}
	}
	out = append(out, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	for _, d := range p.Devices {
		out = append(out, "--dev-bind-try", d, d)
	}
	for _, r := range p.ReadOnly {
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("sandbox read-only path %q must be absolute", r)
		}
		out = append(out, "--ro-bind", r, r)
	}
	if p.WorkDir != "" {
		out = append(out, "--chdir", p.WorkDir)
	}
	return append(append(out, "--"), argv...), nil
}

// Select returns the sandbox for mode ("auto", "bwrap" or "none"). In auto
// mode a missing or non-functional bubblewrap falls back to None with a
// warning; in bwrap mode it is an error.
func Select(mode string) (Sandbox, error) {
	switch mode {
	case "none":
		return None{Reason: "disabled with -sandbox none"}, nil
	case "bwrap", "auto", "":
		b, err := probe()
		if err == nil {
			return b, nil
		}
		if mode == "bwrap" {
			return nil, err
		}
		slog.Warn("bubblewrap unavailable; falling back to an unsandboxed runner", "error", err)
		return None{Reason: err.Error()}, nil
	}
	return nil, fmt.Errorf("unknown sandbox mode %q", mode)
}

// Available reports whether a working bubblewrap is present.
func Available() error {
	_, err := probe()
	return err
}

func probe() (Bwrap, error) {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return Bwrap{}, fmt.Errorf("bwrap not found: %w", err)
	}
	b := Bwrap{Path: path}
	argv, _ := b.Wrap([]string{"true"}, Policy{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput(); err != nil {
		return Bwrap{}, fmt.Errorf("bwrap cannot create a sandbox here: %v: %s", err, out)
	}
	return b, nil
}

func firstArg(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return argv[0]
}
