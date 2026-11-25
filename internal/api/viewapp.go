package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flipperdevices/flipctl/internal/assistant"
	"github.com/flipperdevices/flipctl/internal/plugin"
	"github.com/flipperdevices/flipctl/internal/runner"
	"github.com/flipperdevices/flipctl/internal/viewdoc"
)

// assistantTimeout bounds one assistant round trip.
var assistantTimeout = 90 * time.Second

type assistantNote struct {
	Mode  assistant.Mode
	Text  string
	Error bool
}

func viewKey(sessionID, appID string) string { return sessionID + "\x00" + appID }

// effectiveCapabilities are the capabilities the daemon enforces and asks the
// user to confirm. A ViewApp gets the union of what its run targets were
// granted, plus the agent capability if any action talks to the assistant,
// so a generated view can never under-declare.
func (s *Server) effectiveCapabilities(a plugin.App) plugin.Capabilities {
	if a.Kind != plugin.KindViewApp {
		return a.Capabilities.Union(plugin.Capabilities{})
	}
	caps := plugin.Capabilities{Agent: a.UsesAssistant()}
	snap := s.registry.Snapshot()
	for _, id := range a.RunTargets() {
		if t, ok := snap.ByID(id); ok {
			caps = caps.Union(t.Granted)
		}
	}
	return caps
}

// checkRuntimePolicy applies rules that only make sense for apps that did not
// ship with the image: executables must live under a command root, and
// ViewApps may only run active CommandApps.
func (s *Server) checkRuntimePolicy(a plugin.App) error {
	if a.Kind != plugin.KindViewApp {
		return checkExecutableRoots(a.Command.Executable, s.opts.CommandRoots)
	}
	snap := s.registry.Snapshot()
	for _, id := range a.RunTargets() {
		t, ok := snap.ByID(id)
		if !ok {
			return fmt.Errorf("run target %q is not registered", id)
		}
		if t.App.Kind == plugin.KindViewApp {
			return fmt.Errorf("run target %q is a ViewApp; views can only run command apps", id)
		}
	}
	if a.UsesAssistant() && s.opts.Assistant == nil {
		slog.Warn("view app uses the assistant but no provider is configured; its assistant actions stay disabled", "id", a.ID)
	}
	return nil
}

// handleViewActionLocked runs one ViewApp button for session ss.
func (s *Server) handleViewActionLocked(ss *Session, actionID string, vals map[string]string) (int, string, error) {
	entry, ok := s.registry.Snapshot().ByID(ss.CurrentAppID)
	if !ok || entry.App.Kind != plugin.KindViewApp {
		return http.StatusBadRequest, "invalid_action", fmt.Errorf("current view is not a ViewApp")
	}
	act, ok := entry.App.ActionByID(actionID)
	if !ok {
		return http.StatusBadRequest, "invalid_action", fmt.Errorf("unknown view action %q", actionID)
	}
	if ss.FormValues == nil {
		ss.FormValues = map[string]string{}
	}
	for k, v := range vals {
		ss.FormValues[k] = v
	}
	form := map[string]string{}
	for _, f := range entry.App.Fields {
		form[f.ID] = firstNonEmpty(ss.FormValues[f.ID], f.Default)
	}
	key := viewKey(ss.ID, entry.App.ID)
	if act.Run != nil {
		target, err := s.runTargetLocked(entry, act.Run.Plugin)
		if err != nil {
			return http.StatusConflict, "run_target_unavailable", err
		}
		fields := map[string]string{}
		for k, tmpl := range act.Run.Args {
			fields[k] = expandTemplate(tmpl, form)
		}
		if _, err := runner.Build(target.App, fields); err != nil {
			return http.StatusBadRequest, "validation_failed", err
		}
		if ok, wait := s.limiter.allow(target.App.ID); !ok {
			return http.StatusTooManyRequests, "rate_limited", fmt.Errorf("too many jobs for %s; retry in %ds", target.App.ID, int(wait/time.Second)+1)
		}
		job := s.startJobLocked(target, fields, ss.ID, entry.App.ID)
		s.viewJobs[key] = job.ID
		delete(s.notes, key)
		ss.Revision++
		s.viewChangedLocked(ss)
		return 0, "", nil
	}
	if s.opts.Assistant == nil {
		return http.StatusConflict, "assistant_unavailable", fmt.Errorf("no assistant provider is configured")
	}
	if s.asking[key] {
		return http.StatusConflict, "assistant_busy", fmt.Errorf("an assistant request for this view is already running")
	}
	if ok, wait := s.limiter.allow(entry.App.ID + "#assistant"); !ok {
		return http.StatusTooManyRequests, "rate_limited", fmt.Errorf("too many assistant requests for %s; retry in %ds", entry.App.ID, int(wait/time.Second)+1)
	}
	p := act.PromptBinding()
	req := assistant.Request{Mode: assistant.Mode(act.Mode()), Prompt: expandTemplate(p.Prompt, form), AppID: entry.App.ID}
	for _, c := range p.Context {
		switch c {
		case "form":
			req.Form = form
		case "lastJob":
			if jb, ok := s.jobs[s.viewJobs[key]]; ok {
				req.LastJob = &assistant.JobContext{AppID: jb.AppID, State: jb.State, Output: tail(s.jobOutputLocked(jb), 8000)}
			}
		}
	}
	if req.Mode == assistant.ModeRevise {
		req.Manifest = string(entry.Manifest)
	}
	s.asking[key] = true
	ss.Revision++
	s.viewChangedLocked(ss)
	go s.completeAssistant(ss.ID, entry.App.ID, req)
	return 0, "", nil
}

func (s *Server) completeAssistant(sessionID, appID string, req assistant.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), assistantTimeout)
	defer cancel()
	provider := s.opts.Assistant
	reply, err := provider.Complete(ctx, req)
	s.mu.Lock()
	defer s.mu.Unlock()
	key := viewKey(sessionID, appID)
	delete(s.asking, key)
	note := assistantNote{Mode: req.Mode, Text: reply.Text}
	if err != nil {
		note = assistantNote{Mode: req.Mode, Text: "Request failed: " + err.Error(), Error: true}
	} else if req.Mode == assistant.ModeRevise && len(reply.Manifest) > 0 {
		// A revision is an ordinary registration: same parser, same policy,
		// same confirmation gate for capability escalation.
		note.Text = strings.TrimSpace(note.Text + " " + s.applyRevisionLocked(appID, reply.Manifest, provider.Name()))
	}
	s.notes[key] = note
	slog.Info("assistant reply", "provider", provider.Name(), "app", appID, "mode", req.Mode, "error", err != nil)
	if ss, ok := s.sessionsStore[sessionID]; ok && ss.CurrentAppID == appID {
		ss.Revision++
		s.viewChangedLocked(ss)
	}
}

func (s *Server) applyRevisionLocked(appID string, manifest []byte, provider string) string {
	a, err := plugin.Parse(manifest)
	if err != nil {
		return "Proposed revision was rejected: " + err.Error()
	}
	if a.ID != appID {
		return "Proposed revision was rejected: it changes the app id."
	}
	status, _, err := s.registerLocked(a, manifest, "assistant:"+provider)
	switch {
	case err != nil:
		return "Proposed revision was rejected: " + err.Error()
	case status == http.StatusAccepted:
		return "The revision needs more capabilities and is waiting for device confirmation."
	}
	return "The view was updated to v" + a.Version + "."
}

// runTargetLocked returns the active CommandApp a ViewApp action runs, as long
// as the ViewApp's grant still covers what the target was granted.
func (s *Server) runTargetLocked(view plugin.Entry, id string) (plugin.Entry, error) {
	t, ok := s.registry.Snapshot().ByID(id)
	if !ok || t.App.Kind == plugin.KindViewApp {
		return plugin.Entry{}, fmt.Errorf("run target %s is not registered", id)
	}
	if !view.Granted.Covers(t.Granted) {
		return plugin.Entry{}, fmt.Errorf("run target %s now uses %s; re-register %s to approve it", id, t.Granted.String(), view.App.ID)
	}
	return t, nil
}

// startJobLocked creates and launches a job for entry. Callers validate fields
// and apply rate limits first.
func (s *Server) startJobLocked(entry plugin.Entry, fields map[string]string, sessionID, viewAppID string) Job {
	s.next++
	id := "j-" + strconv.Itoa(s.next)
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().Format(time.RFC3339)
	job := Job{ID: id, JobID: id, AppID: entry.App.ID, PluginVersion: entry.App.Version, ViewAppID: viewAppID, SessionID: sessionID, State: "running", Created: now, Updated: now}
	s.jobs[id] = job
	s.cancels[id] = cancel
	s.evLocked("job.started", &job, "", "")
	go s.runJob(ctx, id, entry, fields)
	return job
}

// touchViewJobLocked refreshes sessions whose ViewApp displays job id.
func (s *Server) touchViewJobLocked(jb Job) {
	if jb.ViewAppID == "" {
		return
	}
	for _, ss := range s.sessionsStore {
		if ss.CurrentAppID == jb.ViewAppID && ss.SelectedJobID == "" && s.viewJobs[viewKey(ss.ID, jb.ViewAppID)] == jb.ID {
			ss.Revision++
			s.viewChangedLocked(ss)
		}
	}
}

func (s *Server) jobOutputLocked(jb Job) string {
	if jb.State == "running" {
		if b := s.liveOutput[jb.ID]; b != nil {
			return b.String()
		}
		return ""
	}
	return jb.Result.Stdout
}

func (s *Server) projectViewAppLocked(ss *Session, entry plugin.Entry) viewdoc.Document {
	a := entry.App
	doc := viewdoc.Document{APIVersion: viewdoc.CanonicalAPIVersion, SessionID: ss.ID, Revision: ss.Revision, ViewID: a.ID + "-view", Title: a.Title}
	if a.Summary != "" {
		doc.Blocks = append(doc.Blocks, viewdoc.Block{ID: "summary", Kind: "notice", Severity: "info", Text: a.Summary})
	}
	if entry.Tier == plugin.TierEphemeral {
		doc.Blocks = append(doc.Blocks, ephemeralNotice(entry))
	}
	if len(a.Fields) > 0 {
		fields := []viewdoc.FormField{}
		for _, f := range a.Fields {
			fields = append(fields, viewdoc.FormField{ID: f.ID, Label: f.Label, Type: f.Type, Default: firstNonEmpty(ss.FormValues[f.ID], f.Default), Required: f.Required, Validation: f.Pattern})
		}
		doc.Blocks = append(doc.Blocks, viewdoc.Block{ID: "form", Kind: "form", Title: a.Title + " options", Fields: fields})
	}
	key := viewKey(ss.ID, a.ID)
	jb, hasJob := s.jobs[s.viewJobs[key]]
	running := hasJob && jb.State == "running"
	var records []map[string]any
	var lines []string
	if hasJob {
		for _, l := range strings.Split(s.jobOutputLocked(jb), "\n") {
			if strings.TrimSpace(l) == "" {
				continue
			}
			lines = append(lines, l)
			var m map[string]any
			if json.Unmarshal([]byte(l), &m) == nil {
				records = append(records, m)
			}
		}
	}
	if hasJob {
		doc.Blocks = append(doc.Blocks, viewdoc.Block{ID: "last-job", Kind: "key_value", Title: "Last run", Pairs: []viewdoc.KeyValuePair{{Key: "jobId", Value: jb.ID}, {Key: "appId", Value: jb.AppID}, {Key: "state", Value: jb.State}, {Key: "records", Value: strconv.Itoa(len(records))}}})
	}
	if running {
		doc.Blocks = append(doc.Blocks, viewdoc.Block{ID: "progress", Kind: "progress", Progress: &viewdoc.Progress{Label: "Running " + jb.AppID, Text: fmt.Sprintf("%d records so far", len(records))}})
	}
	for i, d := range a.Display {
		id := fmt.Sprintf("display-%d", i+1)
		limit := d.Limit
		if limit == 0 {
			limit = 10
		}
		title := firstNonEmpty(d.Title, "Last run")
		if !hasJob {
			if i == 0 {
				doc.Blocks = append(doc.Blocks, viewdoc.Block{ID: id, Kind: "text", Title: title, Text: "No data yet."})
			}
			continue
		}
		switch d.As {
		case "table":
			cols := d.Columns
			if len(cols) == 0 && len(records) > 0 {
				for k := range records[0] {
					cols = append(cols, k)
				}
				sort.Strings(cols)
			}
			// An empty table would serialize without rows (omitempty) and
			// is not useful anyway, so show a placeholder until data arrives.
			if len(cols) == 0 || len(records) == 0 {
				doc.Blocks = append(doc.Blocks, viewdoc.Block{ID: id, Kind: "text", Title: title, Text: "Waiting for records..."})
				continue
			}
			b := viewdoc.Block{ID: id, Kind: "table", Title: fmt.Sprintf("%s (last %d of %d)", title, min(limit, len(records)), len(records)), Rows: []viewdoc.TableRow{}}
			for _, c := range cols {
				b.Columns = append(b.Columns, viewdoc.TableColumn{ID: c, Label: c})
			}
			for _, r := range records[max(0, len(records)-limit):] {
				cells := map[string]string{}
				for _, c := range cols {
					if v, ok := r[c]; ok {
						cells[c] = formatCell(v)
					}
				}
				b.Rows = append(b.Rows, viewdoc.TableRow{Cells: cells})
			}
			doc.Blocks = append(doc.Blocks, b)
		case "log":
			b := viewdoc.Block{ID: id, Kind: "log", Title: title}
			start := max(0, len(lines)-limit)
			for j, l := range lines[start:] {
				b.Lines = append(b.Lines, viewdoc.LogLine{ID: fmt.Sprintf("l-%d", start+j+1), Source: "stdout", Text: l})
			}
			if len(b.Lines) == 0 {
				b = viewdoc.Block{ID: id, Kind: "text", Title: title, Text: "No output yet."}
			}
			doc.Blocks = append(doc.Blocks, b)
		}
	}
	providerName := "Assistant"
	if s.opts.Assistant != nil {
		providerName = s.opts.Assistant.DisplayName()
	}
	if s.asking[key] {
		doc.Blocks = append(doc.Blocks, viewdoc.Block{ID: "assistant-progress", Kind: "progress", Progress: &viewdoc.Progress{Label: "Asking " + providerName}})
	}
	if n, ok := s.notes[key]; ok {
		sev := "info"
		if n.Error {
			sev = "error"
		}
		doc.Blocks = append(doc.Blocks, viewdoc.Block{ID: "assistant-reply", Kind: "notice", Severity: sev, Title: providerName, Text: n.Text})
	}
	primary := true
	for _, act := range a.Actions {
		va := viewdoc.Action{ID: "view." + act.ID, Label: act.Label, Enabled: true, Params: map[string]string{"appId": a.ID, "action": act.ID}}
		if act.Run != nil {
			if _, err := s.runTargetLocked(entry, act.Run.Plugin); err != nil || running {
				va.Enabled = false
			}
			if primary {
				va.Role, primary = "primary", false
			}
		} else {
			va.Enabled = s.opts.Assistant != nil && !s.asking[key]
			if s.opts.Assistant == nil {
				va.Label += " (no assistant configured)"
			}
		}
		doc.Actions = append(doc.Actions, va)
	}
	if running {
		doc.Actions = append(doc.Actions, viewdoc.Action{ID: "job.cancel", Label: "Cancel", Enabled: true, Role: "destructive", Params: map[string]string{"jobId": jb.ID}})
	}
	if len(a.Fields) > 0 {
		doc.Actions = append(doc.Actions, viewdoc.Action{ID: "form.update", Label: "Update form", Enabled: true})
	}
	doc.Actions = append(doc.Actions, viewdoc.Action{ID: "navigation.back", Label: "Back", Enabled: true})
	return doc
}

var templateVar = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.-]+)\s*\}\}`)

func expandTemplate(tmpl string, vals map[string]string) string {
	return templateVar.ReplaceAllStringFunc(tmpl, func(m string) string {
		return vals[templateVar.FindStringSubmatch(m)[1]]
	})
}

func formatCell(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
