package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flipperdevices/flipctl/internal/assistant"
	"github.com/flipperdevices/flipctl/internal/viewdoc"
)

const telemetryScript = `#!/bin/sh
n=3
if [ "$1" = "-n" ]; then n="$2"; fi
i=1
while [ "$i" -le "$n" ]; do
  echo "{\"seq\":$i,\"rssi\":-$((80 - i * 3)),\"snr\":$((4 + i))}"
  i=$((i + 1))
done
`

type viewEnv struct {
	*regEnv
	cmdRoot string
}

func newViewEnv(t *testing.T, provider assistant.Provider) *viewEnv {
	t.Helper()
	cmdRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(cmdRoot, "telemetry"), []byte(telemetryScript), 0o755); err != nil {
		t.Fatal(err)
	}
	e := newRegEnv(t, func(o *Options) {
		o.CommandRoots = append(o.CommandRoots, cmdRoot)
		o.Assistant = provider
	})
	return &viewEnv{regEnv: e, cmdRoot: cmdRoot}
}

func (v *viewEnv) targetManifest(version, caps string) string {
	return fmt.Sprintf(`apiVersion: flipctl.app/v1alpha1
kind: CommandApp
metadata: {id: field.rx, title: Field receiver, version: %s}
form:
  - {id: count, label: Count, type: int, default: "3", min: 1, max: 50}
execution:
  executable: %s
  args: ["-n", "{{count}}"]
  timeoutSeconds: 10
capabilities: {%s}
`, version, filepath.Join(v.cmdRoot, "telemetry"), caps)
}

const viewManifest = `apiVersion: flipctl.app/v1alpha1
kind: ViewApp
metadata: {id: assistant.c1.cal, title: Calibration, summary: Generated calibration view, version: 0.1.0}
owner: {plugin: assistant, conversation: c1}
form:
  - {id: packets, label: Packets, type: int, default: "4", min: 1, max: 50}
actions:
  - id: capture
    label: Capture
    run: {plugin: field.rx, args: {count: "{{packets}}"}}
  - id: advise
    label: Best angle?
    ask: {prompt: "Which reading is strongest?", context: [lastJob, form]}
display:
  - {from: lastJob, as: table, columns: [seq, rssi, snr]}
`

func (v *viewEnv) confirm(session Session, id string) {
	v.t.Helper()
	home := v.home(session)
	postSessionActionDoc(v.t, v.ts, session.ID, map[string]any{"actionId": "device.confirm_registration", "viewId": home.ViewID, "revision": home.Revision, "params": map[string]string{"pluginId": id}})
}

func (v *viewEnv) home(session Session) viewdoc.Document {
	v.t.Helper()
	doc := getCanonicalDoc(v.t, v.ts, session.ID)
	if doc.ViewID != "home-app-list" {
		doc = postSessionActionDoc(v.t, v.ts, session.ID, map[string]any{"actionId": "navigation.back", "viewId": doc.ViewID, "revision": doc.Revision})
	}
	return doc
}

func (v *viewEnv) open(session Session, id string) viewdoc.Document {
	v.t.Helper()
	home := v.home(session)
	return postSessionActionDoc(v.t, v.ts, session.ID, map[string]any{"actionId": "app.open", "viewId": home.ViewID, "revision": home.Revision, "params": map[string]string{"appId": id}})
}

func (v *viewEnv) waitView(session Session, cond func(viewdoc.Document) bool) viewdoc.Document {
	v.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		doc := getCanonicalDoc(v.t, v.ts, session.ID)
		if cond(doc) {
			return doc
		}
		if time.Now().After(deadline) {
			v.t.Fatalf("view never reached condition: %#v", doc.Blocks)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func actionByID(doc viewdoc.Document, id string) (viewdoc.Action, bool) {
	for _, a := range doc.Actions {
		if a.ID == id {
			return a, true
		}
	}
	return viewdoc.Action{}, false
}

func (v *viewEnv) setupConfirmedView(session Session) {
	v.t.Helper()
	if s, b := v.put("field.rx", v.targetManifest("0.1.0", "devices: [/dev/ttyLoRa0]")); s != http.StatusAccepted {
		v.t.Fatalf("target %d %v", s, b)
	}
	v.confirm(session, "field.rx")
	s, b := v.put("assistant.c1.cal", viewManifest)
	if s != http.StatusAccepted {
		v.t.Fatalf("view registration should be pending: %d %v", s, b)
	}
	caps := b["plugin"].(map[string]any)["capabilities"].(map[string]any)
	if caps["agent"] != true || fmt.Sprint(caps["devices"]) != "[/dev/ttyLoRa0]" {
		v.t.Fatalf("computed capabilities %v", caps)
	}
	v.confirm(session, "assistant.c1.cal")
}

func TestViewAppCapabilitiesAreComputedFromTargetsAndAssistant(t *testing.T) {
	v := newViewEnv(t, assistant.Scripted{})
	session := createSession(t, v.ts)
	if s, b := v.put("assistant.c1.cal", viewManifest); s != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(b), "not registered") {
		t.Fatalf("view with missing target: %d %v", s, b)
	}
	v.setupConfirmedView(session)
	en, ok := v.entry("assistant.c1.cal")
	if !ok || !en.Granted.Agent || len(en.Granted.Devices) != 1 {
		t.Fatalf("view grant %#v", en.Granted)
	}
}

func TestViewAppRunThenAskLoop(t *testing.T) {
	v := newViewEnv(t, assistant.Scripted{Label: "Claude"})
	session := createSession(t, v.ts)
	v.setupConfirmedView(session)
	doc := v.open(session, "assistant.c1.cal")
	if doc.ViewID != "assistant.c1.cal-view" {
		t.Fatalf("view id %s", doc.ViewID)
	}
	if _, ok := blockByID(doc, "ephemeral-notice"); !ok {
		t.Fatal("generated view not marked ephemeral")
	}
	capture, _ := actionByID(doc, "view.capture")
	if !capture.Enabled || capture.Role != "primary" {
		t.Fatalf("capture action %#v", capture)
	}
	doc = postSessionActionDoc(t, v.ts, session.ID, map[string]any{"actionId": "view.capture", "viewId": doc.ViewID, "revision": doc.Revision, "params": capture.Params})
	doc = v.waitView(session, func(d viewdoc.Document) bool {
		b, ok := blockByID(d, "display-1")
		return ok && b.Kind == "table" && len(b.Rows) == 4 && !docHasKind(d, "progress")
	})
	table, _ := blockByID(doc, "display-1")
	if table.Rows[3].Cells["rssi"] != "-68" || table.Rows[0].Cells["seq"] != "1" {
		t.Fatalf("table rows %#v", table.Rows)
	}
	for _, j := range getJobs(t, v.ts) {
		if j.AppID == "field.rx" && j.ViewAppID != "assistant.c1.cal" {
			t.Fatalf("job not attributed to the view: %#v", j)
		}
	}
	advise, _ := actionByID(doc, "view.advise")
	postSessionActionDoc(t, v.ts, session.ID, map[string]any{"actionId": "view.advise", "viewId": doc.ViewID, "revision": doc.Revision, "params": advise.Params})
	doc = v.waitView(session, func(d viewdoc.Document) bool { _, ok := blockByID(d, "assistant-reply"); return ok })
	reply, _ := blockByID(doc, "assistant-reply")
	if reply.Title != "Claude" || !strings.Contains(reply.Text, "Strongest packet: seq 4 at -68 dBm") {
		t.Fatalf("assistant reply %#v", reply)
	}
}

func TestViewAppRefusesTargetsWhoseGrantGrewAfterApproval(t *testing.T) {
	v := newViewEnv(t, assistant.Scripted{})
	session := createSession(t, v.ts)
	v.setupConfirmedView(session)
	if s, _ := v.put("field.rx", v.targetManifest("0.2.0", "devices: [/dev/ttyLoRa0], network: true")); s != http.StatusAccepted {
		t.Fatalf("escalating target not pending: %d", s)
	}
	v.confirm(session, "field.rx")
	doc := v.open(session, "assistant.c1.cal")
	if a, _ := actionByID(doc, "view.capture"); a.Enabled {
		t.Fatal("capture still enabled after its target gained network access")
	}
	s, b := v.do(http.MethodPost, "/api/v1/sessions/"+session.ID+"/actions", fmt.Sprintf(`{"actionId":"view.capture","viewId":%q,"revision":%d}`, doc.ViewID, doc.Revision), map[string]string{"Content-Type": "application/json"})
	if s != http.StatusConflict || errCode(b) != "run_target_unavailable" {
		t.Fatalf("escalated target run: %d %v", s, b)
	}
}

func TestViewAppAssistantActionsDisabledWithoutProvider(t *testing.T) {
	v := newViewEnv(t, nil)
	session := createSession(t, v.ts)
	v.setupConfirmedView(session)
	doc := v.open(session, "assistant.c1.cal")
	a, _ := actionByID(doc, "view.advise")
	if a.Enabled || !strings.HasSuffix(a.Label, "(no assistant configured)") {
		t.Fatalf("advise without provider %#v", a)
	}
	if c, _ := actionByID(doc, "view.capture"); !c.Enabled {
		t.Fatal("local run action must keep working without an assistant")
	}
}

// reviser proposes a revised manifest for "revise" actions.
type reviser struct{ manifest string }

func (r reviser) Name() string        { return "test-reviser" }
func (r reviser) DisplayName() string { return "Reviser" }
func (r reviser) Complete(ctx context.Context, req assistant.Request) (assistant.Reply, error) {
	return assistant.Reply{Text: "Added a sample count field.", Manifest: []byte(r.manifest)}, nil
}

func TestViewAppReviseGoesThroughRegistration(t *testing.T) {
	revised := strings.Replace(viewManifest, "version: 0.1.0", "version: 0.2.0", 1)
	withRevise := strings.Replace(viewManifest, "display:", `  - id: improve
    label: Improve this view
    revise: {prompt: "Add a sample count field"}
display:`, 1)
	v := newViewEnv(t, reviser{manifest: strings.Replace(revised, "display:", `  - id: improve
    label: Improve this view
    revise: {prompt: "Add a sample count field"}
display:`, 1)})
	session := createSession(t, v.ts)
	if s, _ := v.put("field.rx", v.targetManifest("0.1.0", "devices: [/dev/ttyLoRa0]")); s != http.StatusAccepted {
		t.Fatal("target not pending")
	}
	v.confirm(session, "field.rx")
	v.put("assistant.c1.cal", withRevise)
	v.confirm(session, "assistant.c1.cal")
	doc := v.open(session, "assistant.c1.cal")
	improve, _ := actionByID(doc, "view.improve")
	postSessionActionDoc(t, v.ts, session.ID, map[string]any{"actionId": "view.improve", "viewId": doc.ViewID, "revision": doc.Revision, "params": improve.Params})
	v.waitView(session, func(d viewdoc.Document) bool {
		b, ok := blockByID(d, "assistant-reply")
		return ok && strings.Contains(b.Text, "updated to v0.2.0")
	})
	if en, _ := v.entry("assistant.c1.cal"); en.App.Version != "0.2.0" || !strings.HasPrefix(en.Source, "assistant:test-reviser") {
		t.Fatalf("revision not registered through the API path: %#v", en)
	}
	if _, ok := v.entry("assistant.c1.cal"); !ok {
		t.Fatal("view disappeared")
	}
}
