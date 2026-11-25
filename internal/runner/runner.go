package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/flipperdevices/flipctl/internal/plugin"
	"github.com/flipperdevices/flipctl/internal/sandbox"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type EventCallback func(stream, chunk string)

type Result struct {
	Argv   []string `json:"argv"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`
	Output string   `json:"output"`
	Parsed any      `json:"parsed"`
	Error  string   `json:"error,omitempty"`
	// Sandbox names the sandbox the command ran under, when one was requested.
	Sandbox string `json:"sandbox,omitempty"`
}

// Options controls how a command is executed.
type Options struct {
	// Sandbox wraps the command. Nil runs it directly, as before.
	Sandbox sandbox.Sandbox
	Policy  sandbox.Policy
}

func Build(a plugin.App, vals map[string]string) ([]string, error) {
	v := map[string]string{}
	for _, f := range a.Fields {
		val := vals[f.ID]
		if val == "" {
			val = f.Default
		}
		if f.Required && val == "" {
			return nil, fmt.Errorf("%s required", f.ID)
		}
		if f.Pattern != "" {
			ok, _ := regexp.MatchString(f.Pattern, val)
			if !ok {
				return nil, fmt.Errorf("%s invalid", f.ID)
			}
		}
		if f.Type == "int" && val != "" {
			n, e := strconv.Atoi(val)
			if e != nil || (f.Min != 0 && n < f.Min) || (f.Max != 0 && n > f.Max) {
				return nil, fmt.Errorf("%s out of range", f.ID)
			}
		}
		v[f.ID] = val
	}
	argv := []string{a.Command.Executable}
	for _, t := range a.Command.Args {
		if strings.HasPrefix(t, "{{") && strings.HasSuffix(t, "}}") {
			argv = append(argv, v[strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(t, "{{"), "}}"))])
		} else {
			argv = append(argv, t)
		}
	}
	return argv, nil
}
func Run(ctx context.Context, a plugin.App, vals map[string]string) (Result, error) {
	return RunWithCallback(ctx, a, vals, nil)
}
func RunWithCallback(ctx context.Context, a plugin.App, vals map[string]string, cb EventCallback) (Result, error) {
	return RunWithOptions(ctx, a, vals, cb, Options{})
}

// RunWithOptions runs a with optional sandboxing. Result.Argv always reports
// the logical command, not the sandbox wrapper.
func RunWithOptions(ctx context.Context, a plugin.App, vals map[string]string, cb EventCallback, opts Options) (Result, error) {
	argv, e := Build(a, vals)
	if e != nil {
		return Result{}, e
	}
	execArgv := argv
	if opts.Sandbox != nil {
		if execArgv, e = opts.Sandbox.Wrap(argv, opts.Policy); e != nil {
			return Result{Argv: argv, Error: e.Error(), Sandbox: opts.Sandbox.Name()}, e
		}
	}
	to := time.Duration(a.Command.TimeoutSeconds) * time.Second
	if to == 0 {
		to = 5 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	cmd := exec.CommandContext(cctx, execArgv[0], execArgv[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=", "LANG=C", "LC_ALL=C"}
	cmd.Dir = "."
	killProcessGroupOnCancel(cmd)
	var stdout, stderr bytes.Buffer
	if cb == nil {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		e = cmd.Run()
	} else {
		// Hand exec plain writers instead of reading StdoutPipe/StderrPipe
		// ourselves: Wait then blocks until all output has been copied, so the
		// last lines a command prints before exiting are never dropped.
		// WaitDelay bounds that wait when a canceled command leaves a
		// grandchild holding the pipe open.
		outW := &lineWriter{stream: "stdout", buf: &stdout, cb: cb}
		errW := &lineWriter{stream: "stderr", buf: &stderr, cb: cb}
		cmd.Stdout, cmd.Stderr = outW, errW
		cmd.WaitDelay = waitDelay
		e = cmd.Run()
		outW.flush()
		errW.flush()
	}
	out, errout := limit(stdout.String(), 64000), limit(stderr.String(), 16000)
	r := Result{Argv: argv, Stdout: out, Stderr: errout, Output: limit(out+errout, 64000)}
	if opts.Sandbox != nil {
		r.Sandbox = opts.Sandbox.Name()
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		r.Error = "timeout"
		return r, cctx.Err()
	}
	if errors.Is(cctx.Err(), context.Canceled) {
		r.Error = "canceled"
		return r, cctx.Err()
	}
	if e != nil {
		r.Error = e.Error()
	}
	return r, e
}

// waitDelay is how long Wait keeps copying output after a command is killed.
var waitDelay = 2 * time.Second

// maxLine bounds a single unterminated line, like the previous 1 MiB scanner buffer.
const maxLine = 1024 * 1024

// lineWriter buffers command output and reports it to the callback one line at
// a time, with the same framing as bufio.ScanLines plus a trailing newline.
type lineWriter struct {
	stream  string
	buf     *bytes.Buffer
	cb      EventCallback
	partial []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.emit(w.partial[:i])
		w.partial = w.partial[i+1:]
	}
	if len(w.partial) > maxLine {
		w.emit(w.partial)
		w.partial = nil
	}
	return len(p), nil
}

func (w *lineWriter) emit(line []byte) {
	l := strings.TrimSuffix(string(line), "\r") + "\n"
	w.buf.WriteString(l)
	w.cb(w.stream, l)
}

func (w *lineWriter) flush() {
	if len(w.partial) > 0 {
		w.emit(w.partial)
		w.partial = nil
	}
}

func limit(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
