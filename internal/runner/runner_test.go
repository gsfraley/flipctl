package runner

import (
	"context"
	"github.com/flipperdevices/flipctl/internal/plugin"
	"github.com/flipperdevices/flipctl/internal/sandbox"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildRejectsInvalidAndUsesTemplates(t *testing.T) {
	a := plugin.App{ID: "x", Fields: []plugin.Field{{ID: "target", Required: true, Pattern: `^[a-z]+$`}}, Command: plugin.Command{Executable: "echo", Args: []string{"{{target}}"}}}
	if _, err := Build(a, map[string]string{"target": "bad;"}); err == nil {
		t.Fatal("expected invalid")
	}
	argv, err := Build(a, map[string]string{"target": "ok"})
	if err != nil || argv[1] != "ok" {
		t.Fatalf("%v %#v", err, argv)
	}
}

func TestBuildRealToolsAllowExternalTargets(t *testing.T) {
	apps, err := plugin.LoadDir("../../plugins-real")
	if err != nil {
		t.Fatal(err)
	}
	nmap, ok := plugin.ByID(apps, "network.nmap")
	if !ok {
		t.Fatal("missing nmap")
	}
	argv, err := Build(nmap, map[string]string{"target": "ya.ru"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/nmap", "-sT", "-Pn", "--stats-every", "1s", "-p", "80", "ya.ru"}
	if len(argv) != len(want) {
		t.Fatalf("argv %#v", argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv %#v", argv)
		}
	}
	ping, ok := plugin.ByID(apps, "network.ping")
	if !ok {
		t.Fatal("missing ping")
	}
	argv, err = Build(ping, map[string]string{"target": "ya.ru", "count": "1"})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"/bin/ping", "-n", "-c", "1", "-W", "1", "ya.ru"}
	if len(argv) != len(want) {
		t.Fatalf("argv %#v", argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv %#v", argv)
		}
	}
}

func TestRunWithCallbackStreamsBeforeCompletion(t *testing.T) {
	a := plugin.App{ID: "x", Command: plugin.Command{Executable: "/bin/sh", Args: []string{"-c", "printf 'one\\n'; sleep 0.2; printf 'two\\n'"}, TimeoutSeconds: 2}}
	seen := make(chan string, 2)
	start := time.Now()
	res, err := RunWithCallback(context.Background(), a, nil, func(stream, chunk string) { seen <- chunk })
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "one\ntwo\n" {
		t.Fatalf("bad stdout %q", res.Stdout)
	}
	select {
	case first := <-seen:
		if first != "one\n" || time.Since(start) > time.Second {
			t.Fatalf("not streamed promptly: %q", first)
		}
	default:
		t.Fatal("missing streamed callback")
	}
}

func TestRunWithCallbackKeepsAllOutputLines(t *testing.T) {
	a := plugin.App{ID: "lines", Command: plugin.Command{Executable: "sh", Args: []string{"-c", "i=1; while [ $i -le 40 ]; do echo line$i; echo err$i >&2; i=$((i+1)); done"}}}
	for run := 0; run < 50; run++ {
		var mu sync.Mutex
		got := map[string]int{}
		res, err := RunWithCallback(context.Background(), a, nil, func(stream, chunk string) {
			mu.Lock()
			got[stream]++
			mu.Unlock()
		})
		if err != nil {
			t.Fatal(err)
		}
		// Every line must reach both the result and the streaming callback,
		// including the last ones written right before the process exits.
		if n := strings.Count(res.Stdout, "\n"); n != 40 || got["stdout"] != 40 || got["stderr"] != 40 || !strings.HasSuffix(res.Stdout, "line40\n") {
			t.Fatalf("run %d: stdout lines=%d callbacks=%v tail=%q", run, n, got, tail(res.Stdout))
		}
	}
}

func tail(s string) string {
	if len(s) > 20 {
		return s[len(s)-20:]
	}
	return s
}

func TestCancelKillsChildProcessesPromptly(t *testing.T) {
	a := plugin.App{ID: "slow", Command: plugin.Command{Executable: "sh", Args: []string{"-c", "echo started; sleep 30; echo never"}, TimeoutSeconds: 60}}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	done := make(chan Result)
	go func() {
		r, _ := RunWithCallback(ctx, a, nil, func(stream, chunk string) { once.Do(func() { close(started) }) })
		done <- r
	}()
	<-started
	begin := time.Now()
	cancel()
	select {
	case r := <-done:
		if d := time.Since(begin); d > time.Second || r.Error != "canceled" || strings.Contains(r.Stdout, "never") {
			t.Fatalf("cancel took %v, result %#v", d, r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop the command and its children")
	}
}

type recordingSandbox struct{ got sandbox.Policy }

func (r *recordingSandbox) Name() string { return "recording" }
func (r *recordingSandbox) Wrap(argv []string, p sandbox.Policy) ([]string, error) {
	r.got = p
	return append([]string{"env"}, argv...), nil
}

func TestRunWithOptionsWrapsCommandButReportsLogicalArgv(t *testing.T) {
	a := plugin.App{ID: "t", Command: plugin.Command{Executable: "echo", Args: []string{"hi"}}}
	sb := &recordingSandbox{}
	res, err := RunWithOptions(context.Background(), a, nil, nil, Options{Sandbox: sb, Policy: sandbox.Policy{Devices: []string{"/dev/x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Argv, " ") != "echo hi" || res.Sandbox != "recording" || strings.TrimSpace(res.Stdout) != "hi" {
		t.Fatalf("unexpected result %#v", res)
	}
	if len(sb.got.Devices) != 1 {
		t.Fatalf("policy not passed: %#v", sb.got)
	}
}
