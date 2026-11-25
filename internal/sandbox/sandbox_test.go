package sandbox

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestBwrapWrapOmitsNetworkUnlessDeclared(t *testing.T) {
	b := Bwrap{Path: "/usr/bin/bwrap"}
	argv, err := b.Wrap([]string{"./fakecmds/x", "-n", "3"}, Policy{Devices: []string{"/dev/ttyLoRa0"}, ReadOnly: []string{"/app/fakecmds"}, WorkDir: "/app"})
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(argv, " ")
	if slices.Contains(argv, "--share-net") || !strings.Contains(s, "--unshare-all") {
		t.Fatalf("network shared without declaration: %s", s)
	}
	for _, want := range []string{"--dev /dev", "--dev-bind-try /dev/ttyLoRa0 /dev/ttyLoRa0", "--ro-bind /app/fakecmds /app/fakecmds", "--chdir /app", "-- ./fakecmds/x -n 3"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
	argv, _ = b.Wrap([]string{"x"}, Policy{Network: true})
	if !slices.Contains(argv, "--share-net") {
		t.Fatal("declared network not shared")
	}
	if _, err := b.Wrap([]string{"x"}, Policy{ReadOnly: []string{"relative"}}); err == nil {
		t.Fatal("relative bind accepted")
	}
}

func TestNoneFallbackPassesArgvThrough(t *testing.T) {
	argv, err := None{Reason: "test"}.Wrap([]string{"a", "b"}, Policy{})
	if err != nil || strings.Join(argv, " ") != "a b" {
		t.Fatalf("%v %v", argv, err)
	}
}

func requireBwrap(t *testing.T) Bwrap {
	t.Helper()
	if err := Available(); err != nil {
		t.Skipf("bubblewrap not usable here: %v", err)
	}
	b, _ := probe()
	return b
}

func run(t *testing.T, b Bwrap, p Policy, script string) (string, error) {
	t.Helper()
	argv, err := b.Wrap([]string{"sh", "-c", script}, p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return string(out), err
}

func TestBwrapIsolatesNetworkUnlessDeclared(t *testing.T) {
	b := requireBwrap(t)
	out, err := run(t, b, Policy{}, "tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' '")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.TrimSpace(out) != "lo" {
		t.Fatalf("undeclared network exposed interfaces: %q", out)
	}
}

func TestBwrapRestrictsDevicesAndFilesystem(t *testing.T) {
	b := requireBwrap(t)
	var dev string
	for _, d := range []string{"/dev/kvm", "/dev/fuse", "/dev/net/tun", "/dev/loop-control"} {
		if _, err := os.Stat(d); err == nil {
			dev = d
			break
		}
	}
	if dev == "" {
		t.Skip("no probe device present on host")
	}
	if out, _ := run(t, b, Policy{}, "test -e "+dev+" && echo present || echo absent"); strings.TrimSpace(out) != "absent" {
		t.Fatalf("undeclared %s visible: %q", dev, out)
	}
	if out, _ := run(t, b, Policy{Devices: []string{dev}}, "test -e "+dev+" && echo present || echo absent"); strings.TrimSpace(out) != "present" {
		t.Fatalf("declared %s missing: %q", dev, out)
	}
	home, _ := os.UserHomeDir()
	if out, _ := run(t, b, Policy{}, "test -e '"+home+"' && echo present || echo absent"); strings.TrimSpace(out) != "absent" {
		t.Fatalf("home directory visible in sandbox: %q", out)
	}
}
