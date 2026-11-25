package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flipperdevices/flipctl/internal/plugin"
	"github.com/flipperdevices/flipctl/internal/viewdoc"
)

type regEnv struct {
	t     *testing.T
	srv   *Server
	ts    *httptest.Server
	root  string
	store *plugin.Store
}

func newRegEnv(t *testing.T, mutate func(*Options)) *regEnv {
	t.Helper()
	root, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	apps, err := loadTestPluginApps("plugins")
	if err != nil {
		t.Fatal(err)
	}
	store, err := plugin.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Store: store, CommandRoots: []string{filepath.Join(root, "fakecmds")}, EmulatedDeviceConfirm: true}
	if mutate != nil {
		mutate(&opts)
	}
	srv := NewWithOptions(plugin.NewRegistry(plugin.SystemEntries(apps, "catalog")), opts)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &regEnv{t: t, srv: srv, ts: ts, root: root, store: store}
}

// manifest returns a fake-ping based CommandApp manifest with extra YAML appended.
func (e *regEnv) manifest(id, version, extra string) string {
	return fmt.Sprintf(`apiVersion: flipctl.app/v1alpha1
kind: CommandApp
metadata:
  id: %s
  title: Field probe
  summary: Runtime-registered probe
  version: %s
form:
  - id: target
    label: Target
    type: string
    default: 127.0.0.1
    pattern: '^[A-Za-z0-9_.:-]+$'
execution:
  executable: %s
  args: ["-c", "1", "{{target}}"]
  timeoutSeconds: 10
parser: ping
%s`, id, version, filepath.Join(e.root, "fakecmds", "fake-ping"), extra)
}

func (e *regEnv) do(method, path, body string, hdr map[string]string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func (e *regEnv) put(id, manifest string) (int, map[string]any) {
	return e.do(http.MethodPut, "/api/v1/plugins/"+id, manifest, map[string]string{"Content-Type": "application/yaml"})
}

func errCode(body map[string]any) string {
	if m, ok := body["error"].(map[string]any); ok {
		s, _ := m["code"].(string)
		return s
	}
	return ""
}

func (e *regEnv) entry(id string) (plugin.Entry, bool) {
	return e.srv.Registry().Snapshot().ByID(id)
}

func blockByID(doc viewdoc.Document, id string) (viewdoc.Block, bool) {
	for _, b := range doc.Blocks {
		if b.ID == id {
			return b, true
		}
	}
	return viewdoc.Block{}, false
}

func TestUploadedManifestsGetTheSameValidationAsDiskPlugins(t *testing.T) {
	e := newRegEnv(t, nil)
	good := e.manifest("field.probe", "0.1.0", "")
	cases := []struct {
		name, id, body string
		status         int
		code           string
	}{
		{"unknown key", "field.probe", good + "hooks: {post: reboot}\n", 422, "invalid_manifest"},
		{"shell metacharacters", "field.probe", strings.Replace(good, `"{{target}}"`, `"{{target}};reboot"`, 1), 422, "invalid_manifest"},
		{"not yaml", "field.probe", "{{{", 422, "invalid_manifest"},
		{"id mismatch", "field.other", good, 400, "id_mismatch"},
		{"executable outside command roots", "field.probe", strings.Replace(good, filepath.Join(e.root, "fakecmds", "fake-ping"), "/bin/sh", 1), 422, "registration_rejected"},
		{"PATH lookup", "field.probe", strings.Replace(good, filepath.Join(e.root, "fakecmds", "fake-ping"), "sh", 1), 422, "registration_rejected"},
		{"escape via ..", "field.probe", strings.Replace(good, filepath.Join(e.root, "fakecmds", "fake-ping"), filepath.Join(e.root, "fakecmds", "..", "Makefile"), 1), 422, "registration_rejected"},
		{"shadow system app", "network.ping", strings.Replace(good, "field.probe", "network.ping", 1), 409, "plugin_protected"},
	}
	for _, c := range cases {
		status, body := e.put(c.id, c.body)
		if status != c.status || errCode(body) != c.code {
			t.Errorf("%s: got %d %s, want %d %s (%v)", c.name, status, errCode(body), c.status, c.code, body)
		}
	}
	if v := e.srv.Registry().Snapshot().Version; v != 1 {
		t.Fatalf("rejected uploads changed the registry (version %d)", v)
	}
	status, body := e.put("field.probe", good)
	if status != http.StatusCreated {
		t.Fatalf("valid upload: %d %v", status, body)
	}
	en, ok := e.entry("field.probe")
	if !ok || en.Tier != plugin.TierEphemeral || !strings.HasPrefix(en.Source, "api:127.0.0.1") {
		t.Fatalf("entry %#v", en)
	}
	if _, err := os.Stat(filepath.Join(e.store.Root, "ephemeral", "field.probe.yaml")); err != nil {
		t.Fatalf("ephemeral manifest not stored: %v", err)
	}
	_, list := e.do(http.MethodGet, "/api/v1/plugins", "", nil)
	var tiers []string
	for _, p := range list["plugins"].([]any) {
		pm := p.(map[string]any)
		tiers = append(tiers, fmt.Sprintf("%s=%s/%s", pm["id"], pm["tier"], pm["state"]))
	}
	if strings.Join(tiers, ",") != "network.nmap=system/active,network.ping=system/active,field.probe=ephemeral/active" {
		t.Fatalf("plugin list %v", tiers)
	}
}

func TestReplaceKeepsRunningJobsAndRemoveCancelsThem(t *testing.T) {
	e := newRegEnv(t, nil)
	if s, b := e.put("field.probe", e.manifest("field.probe", "0.1.0", "")); s != 201 {
		t.Fatalf("register %d %v", s, b)
	}
	job := startCanonicalJob(t, e.ts, "field.probe", map[string]string{"target": "slow.test"})
	if job.PluginVersion != "0.1.0" {
		t.Fatalf("job version %q", job.PluginVersion)
	}
	before := e.srv.Registry().Snapshot().Version
	if s, b := e.put("field.probe", e.manifest("field.probe", "0.2.0", "")); s != 200 {
		t.Fatalf("replace %d %v", s, b)
	}
	if en, _ := e.entry("field.probe"); en.App.Version != "0.2.0" || e.srv.Registry().Snapshot().Version != before+1 {
		t.Fatalf("replace not applied atomically: %#v", en.App)
	}
	jobs := waitJobState(t, e.ts, job.ID, "running")
	for _, j := range jobs {
		if j.ID == job.ID && j.PluginVersion != "0.1.0" {
			t.Fatalf("running job switched definitions: %#v", j)
		}
	}
	if s, b := e.do(http.MethodDelete, "/api/v1/plugins/field.probe", "", nil); s != 200 {
		t.Fatalf("delete %d %v", s, b)
	}
	jobs = waitJobState(t, e.ts, job.ID, "canceled")
	for _, j := range jobs {
		if j.ID == job.ID && j.Result.Error != "canceled: plugin removed" {
			t.Fatalf("cancel reason %q", j.Result.Error)
		}
	}
	if _, ok := e.entry("field.probe"); ok {
		t.Fatal("removed plugin still registered")
	}
	if a := e.store.ListArchive(); len(a) != 1 || a[0].Version != "0.2.0" {
		t.Fatalf("removed manifest not archived: %#v", a)
	}
	if s, _ := e.do(http.MethodDelete, "/api/v1/plugins/network.ping", "", nil); s != http.StatusConflict {
		t.Fatalf("system plugin delete status %d", s)
	}
}

func TestSensitiveCapabilitiesRequireDeviceConfirmation(t *testing.T) {
	e := newRegEnv(t, nil)
	session := createSession(t, e.ts)
	sensitive := e.manifest("field.lora", "0.1.0", "capabilities:\n  devices: [/dev/ttyLoRa0]\n")
	status, body := e.put("field.lora", sensitive)
	if status != http.StatusAccepted || body["plugin"].(map[string]any)["state"] != "pending" {
		t.Fatalf("sensitive registration %d %v", status, body)
	}
	if _, ok := e.entry("field.lora"); ok {
		t.Fatal("pending registration is active")
	}
	// The registration API itself cannot confirm: re-uploading stays pending
	// and there is no confirm route.
	if s, _ := e.put("field.lora", sensitive); s != http.StatusAccepted {
		t.Fatalf("re-upload status %d", s)
	}
	for _, path := range []string{"/api/v1/plugins/field.lora/confirm", "/api/v1/plugins/field.lora:confirm"} {
		if s, _ := e.do(http.MethodPost, path, "{}", nil); s != http.StatusNotFound && s != http.StatusMethodNotAllowed {
			t.Fatalf("%s reachable: %d", path, s)
		}
	}
	home := getCanonicalDoc(t, e.ts, session.ID)
	pending, ok := blockByID(home, "pending-registrations")
	if !ok || pending.Items[0].Action.ID != "device.confirm_registration" || !strings.Contains(pending.Items[0].Description, "device /dev/ttyLoRa0") {
		t.Fatalf("pending block %#v", pending)
	}
	confirmed := postSessionActionDoc(t, e.ts, session.ID, map[string]any{"actionId": "device.confirm_registration", "viewId": home.ViewID, "revision": home.Revision, "params": map[string]string{"pluginId": "field.lora"}})
	en, ok := e.entry("field.lora")
	if !ok || len(en.Granted.Devices) != 1 {
		t.Fatalf("confirmation did not activate: %#v", en)
	}
	if _, ok := blockByID(confirmed, "ephemeral-apps"); !ok {
		t.Fatalf("home after confirm lacks ephemeral block: %#v", confirmed.Blocks)
	}
	// Same capabilities: replacement is immediate.
	if s, _ := e.put("field.lora", e.manifest("field.lora", "0.2.0", "capabilities:\n  devices: [/dev/ttyLoRa0]\n")); s != http.StatusOK {
		t.Fatalf("same-capability replace status %d", s)
	}
	// Escalation: pending again, the confirmed version stays active.
	if s, _ := e.put("field.lora", e.manifest("field.lora", "0.3.0", "capabilities:\n  devices: [/dev/ttyLoRa0]\n  rf_transmit: true\n")); s != http.StatusAccepted {
		t.Fatalf("escalating replace status %d", s)
	}
	if en, _ := e.entry("field.lora"); en.App.Version != "0.2.0" || en.Granted.RFTransmit {
		t.Fatalf("escalation applied without confirmation: %#v", en)
	}
	home = getCanonicalDoc(t, e.ts, session.ID)
	postSessionActionDoc(t, e.ts, session.ID, map[string]any{"actionId": "device.reject_registration", "viewId": home.ViewID, "revision": home.Revision, "params": map[string]string{"pluginId": "field.lora"}})
	if en, _ := e.entry("field.lora"); en.App.Version != "0.2.0" {
		t.Fatalf("reject changed the active version: %#v", en.App)
	}
}

func TestDeviceConfirmationActionIsOffWithoutEmulation(t *testing.T) {
	e := newRegEnv(t, func(o *Options) { o.EmulatedDeviceConfirm = false })
	session := createSession(t, e.ts)
	e.put("field.lora", e.manifest("field.lora", "0.1.0", "capabilities:\n  network: true\n"))
	home := getCanonicalDoc(t, e.ts, session.ID)
	pending, _ := blockByID(home, "pending-registrations")
	if pending.Items[0].Action.Enabled {
		t.Fatal("confirm offered without an emulated device button")
	}
	s, _ := e.do(http.MethodPost, "/api/v1/sessions/"+session.ID+"/actions", fmt.Sprintf(`{"actionId":"device.confirm_registration","viewId":%q,"revision":%d,"params":{"pluginId":"field.lora"}}`, home.ViewID, home.Revision), map[string]string{"Content-Type": "application/json"})
	if s != http.StatusBadRequest {
		t.Fatalf("confirm without emulation status %d", s)
	}
	if _, ok := e.entry("field.lora"); ok {
		t.Fatal("activated without a device confirmation")
	}
}

func TestRegistrationIsLocalOnlyAndRefusesCrossOrigin(t *testing.T) {
	e := newRegEnv(t, nil)
	h := e.srv.Handler()
	m := e.manifest("field.probe", "0.1.0", "")
	try := func(remote, host, origin string) int {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/plugins/field.probe", strings.NewReader(m))
		req.RemoteAddr, req.Host = remote, host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("registration route sent CORS headers")
		}
		return w.Code
	}
	if c := try("192.168.1.20:5000", "flipper.local:8080", ""); c != http.StatusForbidden {
		t.Fatalf("remote client: %d", c)
	}
	if c := try("127.0.0.1:5000", "rebind.example:8080", ""); c != http.StatusForbidden {
		t.Fatalf("DNS-rebinding host: %d", c)
	}
	if c := try("127.0.0.1:5000", "localhost:8080", "http://evil.example"); c != http.StatusForbidden {
		t.Fatalf("cross-origin browser request: %d", c)
	}
	if c := try("[::1]:5000", "localhost:8080", "http://localhost:8080"); c != http.StatusCreated {
		t.Fatalf("same-origin local request: %d", c)
	}
	e.srv.opts.AllowRemoteRegistration = true
	if c := try("192.168.1.20:5000", "flipper.local:8080", ""); c != http.StatusOK {
		t.Fatalf("remote with -allow-remote-registration: %d", c)
	}
	if c := try("192.168.1.20:5000", "flipper.local:8080", "http://evil.example"); c != http.StatusForbidden {
		t.Fatalf("cross-origin still refused with remote allowed: %d", c)
	}
	// The emulated device button cannot be pressed from another origin.
	session := createSession(t, e.ts)
	s, _ := e.do(http.MethodPost, "/api/v1/sessions/"+session.ID+"/actions", `{"actionId":"device.confirm_registration","viewId":"home-app-list","revision":1,"params":{"pluginId":"x"}}`, map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"})
	if s != http.StatusForbidden {
		t.Fatalf("cross-origin device action status %d", s)
	}
}

func TestRegistrationDisabledWithoutStore(t *testing.T) {
	ts := testServer()
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/plugins/x", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestPromotionNeedsConfirmationAndMovesToInstalled(t *testing.T) {
	e := newRegEnv(t, nil)
	session := createSession(t, e.ts)
	e.put("field.probe", e.manifest("field.probe", "0.1.0", ""))
	if s, b := e.do(http.MethodPost, "/api/v1/plugins/field.probe/promote", "", nil); s != http.StatusAccepted {
		t.Fatalf("promote %d %v", s, b)
	}
	if en, _ := e.entry("field.probe"); en.Tier != plugin.TierEphemeral {
		t.Fatal("promoted without confirmation")
	}
	home := getCanonicalDoc(t, e.ts, session.ID)
	postSessionActionDoc(t, e.ts, session.ID, map[string]any{"actionId": "device.confirm_registration", "viewId": home.ViewID, "revision": home.Revision, "params": map[string]string{"pluginId": "field.probe"}})
	en, _ := e.entry("field.probe")
	if en.Tier != plugin.TierInstalled {
		t.Fatalf("tier after confirm %s", en.Tier)
	}
	if _, err := os.Stat(filepath.Join(e.store.Root, "installed", "field.probe.yaml")); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.put("field.probe", e.manifest("field.probe", "0.2.0", "")); s != http.StatusConflict {
		t.Fatalf("ephemeral upload over installed status %d", s)
	}
	home = getCanonicalDoc(t, e.ts, session.ID)
	if _, ok := blockByID(home, "ephemeral-apps"); ok {
		t.Fatal("installed app still listed as ephemeral")
	}
}

func TestRestoreFromArchiveGoesThroughTheGateAgain(t *testing.T) {
	e := newRegEnv(t, nil)
	session := createSession(t, e.ts)
	e.put("field.lora", e.manifest("field.lora", "0.1.0", "owner: {plugin: assistant, conversation: c-12}\ncapabilities:\n  devices: [/dev/ttyLoRa0]\n"))
	home := getCanonicalDoc(t, e.ts, session.ID)
	postSessionActionDoc(t, e.ts, session.ID, map[string]any{"actionId": "device.confirm_registration", "viewId": home.ViewID, "revision": home.Revision, "params": map[string]string{"pluginId": "field.lora"}})
	e.do(http.MethodDelete, "/api/v1/plugins/field.lora", "", nil)
	_, list := e.do(http.MethodGet, "/api/v1/plugins?archived=1", "", nil)
	arch := list["archived"].([]any)
	if len(arch) != 1 || arch[0].(map[string]any)["owner"] != "assistant.c-12" {
		t.Fatalf("archive listing %v", arch)
	}
	s, b := e.do(http.MethodPost, "/api/v1/plugins/field.lora/restore", "", nil)
	if s != http.StatusAccepted {
		t.Fatalf("restore of a sensitive plugin must need re-approval: %d %v", s, b)
	}
	if _, ok := e.entry("field.lora"); ok {
		t.Fatal("restore bypassed confirmation")
	}
}

func TestKeptEphemeralRegistrationsLoseGrantsAcrossRestart(t *testing.T) {
	e := newRegEnv(t, nil)
	plain, _ := plugin.Parse([]byte(e.manifest("field.plain", "1.0.0", "")))
	lora, _ := plugin.Parse([]byte(e.manifest("field.lora", "1.0.0", "capabilities:\n  network: true\n")))
	e.srv.RestoreEphemeral([]plugin.Entry{
		{App: plain, Manifest: []byte(e.manifest("field.plain", "1.0.0", ""))},
		{App: lora, Manifest: []byte(e.manifest("field.lora", "1.0.0", "capabilities:\n  network: true\n"))},
	})
	if _, ok := e.entry("field.plain"); !ok {
		t.Fatal("non-sensitive kept plugin not restored")
	}
	if _, ok := e.entry("field.lora"); ok {
		t.Fatal("sensitive kept plugin restored without re-approval")
	}
	if _, ok := e.srv.pending["field.lora"]; !ok {
		t.Fatal("sensitive kept plugin not pending")
	}
}

func TestRegistryChangedEventRefreshesHomeSessions(t *testing.T) {
	e := newRegEnv(t, nil)
	session := createSession(t, e.ts)
	e.put("field.probe", e.manifest("field.probe", "0.1.0", ""))
	resp, err := http.Get(e.ts.URL + "/api/v1/events?replayOnly=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sawRegistry, sawView bool
	for _, ev := range parseSSEEvents(t, resp.Body) {
		if ev.Type == "registry.changed" && ev.Registry != nil && ev.Registry.PluginID == "field.probe" && ev.Registry.Change == "added" && ev.Registry.Version == 2 {
			sawRegistry = true
		}
		if ev.Type == "view.changed" && ev.SessionID == session.ID && ev.Revision == 2 {
			sawView = true
		}
	}
	if !sawRegistry || !sawView {
		t.Fatalf("registry.changed=%v view.changed=%v", sawRegistry, sawView)
	}
	home := getCanonicalDoc(t, e.ts, session.ID)
	eph, ok := blockByID(home, "ephemeral-apps")
	if !ok || !strings.HasPrefix(eph.Items[0].Label, "[ephemeral] ") {
		t.Fatalf("ephemeral app not marked: %#v", home.Blocks)
	}
	opened := postSessionActionDoc(t, e.ts, session.ID, map[string]any{"actionId": "app.open", "viewId": home.ViewID, "revision": home.Revision, "params": map[string]string{"appId": "field.probe"}})
	if _, ok := blockByID(opened, "ephemeral-notice"); !ok {
		t.Fatal("ephemeral app view lacks its tier notice")
	}
}

func TestPluginManifestSchemaIsServed(t *testing.T) {
	ts := testServer()
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/schema/plugin-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || doc["$id"] != "https://flipctl.dev/schemas/plugin-manifest.json" {
		t.Fatalf("schema %v %v", doc["$id"], err)
	}
}

func TestExecutableRootsMayBeRelativeToWorkingDirectory(t *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := checkExecutableRoots("./fakecmds/fake-ping", []string{"fakecmds"}); err != nil {
		t.Fatalf("relative root rejected a contained executable: %v", err)
	}
	if err := checkExecutableRoots("./Makefile", []string{"fakecmds"}); err == nil {
		t.Fatal("relative root accepted an executable outside it")
	}
}

func TestEphemeralNoticeDoesNotOverclaimSandboxing(t *testing.T) {
	e := newRegEnv(t, nil)
	session := createSession(t, e.ts)
	e.put("field.probe", e.manifest("field.probe", "0.1.0", ""))
	notice, _ := blockByID(getCanonicalDoc(t, e.ts, session.ID), "ephemeral-notice")
	if !strings.Contains(notice.Text, "WITHOUT a sandbox") {
		t.Fatalf("notice claims sandboxing without a sandbox: %q", notice.Text)
	}
}

func TestOpenAPIDocumentsRegistrationRoutes(t *testing.T) {
	paths := objectAt(t, readCheckedInOpenAPI(t), "paths")
	for path, methods := range map[string][]string{
		"/api/v1/plugins":                     {"get"},
		"/api/v1/plugins/{pluginId}":          {"get", "put", "delete"},
		"/api/v1/plugins/{pluginId}/promote":  {"post"},
		"/api/v1/plugins/{pluginId}/restore":  {"post"},
		"/api/v1/schema/plugin-manifest.json": {"get"},
	} {
		for _, m := range methods {
			if stringValue(objectValue(t, objectValue(t, paths, path), m), "operationId") == "" {
				t.Fatalf("%s %s missing operationId", m, path)
			}
		}
	}
	put := objectValue(t, objectValue(t, paths, "/api/v1/plugins/{pluginId}"), "put")
	for _, status := range []string{"201", "202", "403", "422"} {
		if _, ok := objectValue(t, put, "responses")[status]; !ok {
			t.Fatalf("PUT plugin missing %s response", status)
		}
	}
}
