package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/flipperdevices/flipctl/internal/assistant"
	"github.com/flipperdevices/flipctl/internal/plugin"
	"github.com/flipperdevices/flipctl/internal/sandbox"
)

// Options configures runtime registration, sandboxing and limits. The zero
// value keeps PR #4 behavior: a fixed catalog, no registration API and no
// sandbox.
type Options struct {
	Static http.Handler
	// Store enables the runtime registration API. Nil disables it.
	Store *plugin.Store
	// CommandRoots are directories that executables of installed and
	// ephemeral apps must resolve into.
	CommandRoots []string
	// Sandbox wraps commands of installed and ephemeral apps (and of system
	// apps when SandboxSystem is set). Nil runs them unsandboxed, with a
	// warning per job.
	Sandbox       sandbox.Sandbox
	SandboxSystem bool
	// JobRate is the sustained number of job starts per minute allowed for
	// each plugin, with bursts up to JobBurst. Zero disables the limit.
	JobRate  float64
	JobBurst int
	// AllowRemoteRegistration accepts registration writes from non-loopback
	// clients. Cross-origin browser requests are always rejected.
	AllowRemoteRegistration bool
	// EmulatedDeviceConfirm exposes the device confirmation as a session
	// action that stands in for a physical button press. Without it,
	// sensitive registrations stay pending until a DeviceConfirmer adapter
	// confirms them.
	EmulatedDeviceConfirm bool
	// Assistant answers ViewApp ask/revise/act actions. Nil disables them.
	Assistant assistant.Provider
}

// Pending is a registration or promotion waiting for device confirmation.
type Pending struct {
	Kind         string              `json:"kind"`
	App          plugin.App          `json:"-"`
	Manifest     []byte              `json:"-"`
	Capabilities plugin.Capabilities `json:"capabilities"`
	Source       string              `json:"source"`
	Requested    time.Time           `json:"requested"`
	Replaces     string              `json:"replacesVersion,omitempty"`
}

const (
	pendingRegister = "register"
	pendingPromote  = "promote"
)

// RegistryChange is attached to registry.changed events.
type RegistryChange struct {
	Version  uint64      `json:"version"`
	PluginID string      `json:"pluginId"`
	Change   string      `json:"change"`
	Tier     plugin.Tier `json:"tier,omitempty"`
}

type pluginInfo struct {
	ID           string               `json:"id"`
	Title        string               `json:"title"`
	Version      string               `json:"version,omitempty"`
	Kind         string               `json:"kind"`
	Tier         plugin.Tier          `json:"tier"`
	Source       string               `json:"source"`
	State        string               `json:"state"`
	Capabilities plugin.Capabilities  `json:"capabilities"`
	Granted      *plugin.Capabilities `json:"granted,omitempty"`
	Owner        *plugin.Owner        `json:"owner,omitempty"`
	Pending      *Pending             `json:"pending,omitempty"`
	Updated      *time.Time           `json:"updated,omitempty"`
}

type registrationResult struct {
	RegistryVersion uint64     `json:"registryVersion"`
	Plugin          pluginInfo `json:"plugin"`
	Message         string     `json:"message,omitempty"`
}

var errRegistrationDisabled = errors.New("runtime registration is disabled")

func (s *Server) registrationRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/plugins", s.pluginsList)
	m.HandleFunc("GET /api/v1/plugins/{id}", s.pluginGet)
	m.HandleFunc("PUT /api/v1/plugins/{id}", s.localOnly(s.pluginPut))
	m.HandleFunc("DELETE /api/v1/plugins/{id}", s.localOnly(s.pluginDelete))
	m.HandleFunc("POST /api/v1/plugins/{id}/promote", s.localOnly(s.pluginPromote))
	m.HandleFunc("POST /api/v1/plugins/{id}/restore", s.localOnly(s.pluginRestore))
}

// localOnly guards registration writes. By default only loopback clients
// addressing the daemon by a local host name may register, and browser
// requests from another origin are always refused (the rest of the API sends
// a wildcard CORS header, so this also blocks drive-by registration and DNS
// rebinding from web pages).
func (s *Server) localOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Store == nil {
			writeErr(w, http.StatusServiceUnavailable, "registration_disabled", errRegistrationDisabled.Error(), "")
			return
		}
		if !s.opts.AllowRemoteRegistration && (!isLoopbackAddr(r.RemoteAddr) || !isLocalHost(r.Host)) {
			writeErr(w, http.StatusForbidden, "registration_local_only", "plugin registration is only accepted from localhost; start flipctld with -allow-remote-registration to change this", "")
			return
		}
		if crossOrigin(r) {
			writeErr(w, http.StatusForbidden, "cross_origin", "cross-origin registration requests are refused", "")
			return
		}
		h(w, r)
	}
}

func isLoopbackAddr(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLocalHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func crossOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err != nil || u.Host != r.Host
}

func (s *Server) pluginsList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	snap := s.registry.Snapshot()
	out := map[string]any{"registryVersion": snap.Version, "registration": s.opts.Store != nil, "sandbox": s.sandboxName()}
	plugins := []pluginInfo{}
	for _, e := range snap.Entries {
		plugins = append(plugins, s.infoForEntry(e))
	}
	for _, id := range sortedKeys(s.pending) {
		if _, active := snap.ByID(id); !active {
			plugins = append(plugins, infoForPending(s.pending[id]))
		}
	}
	out["plugins"] = plugins
	s.mu.Unlock()
	if r.URL.Query().Get("archived") == "1" && s.opts.Store != nil {
		out["archived"] = s.opts.Store.ListArchive()
	}
	j(w, out)
}

func (s *Server) pluginGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.registry.Snapshot().ByID(id); ok {
		j(w, map[string]any{"plugin": s.infoForEntry(e), "manifest": string(e.Manifest)})
		return
	}
	if p, ok := s.pending[id]; ok {
		j(w, map[string]any{"plugin": infoForPending(p), "manifest": string(p.Manifest)})
		return
	}
	writeErr(w, http.StatusNotFound, "plugin_not_found", "plugin not found", "id")
}

func (s *Server) pluginPut(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, plugin.MaxManifestBytes+1))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "manifest_too_large", err.Error(), "")
		return
	}
	s.register(w, r.PathValue("id"), b, "api:"+clientHost(r))
}

func (s *Server) pluginRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Ref string `json:"ref"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), "")
			return
		}
	}
	b, err := s.opts.Store.ReadArchive(id, req.Ref)
	if err != nil {
		writeErr(w, http.StatusNotFound, "archive_not_found", "no archived manifest for this plugin", "ref")
		return
	}
	s.register(w, id, b, "restore:"+firstNonEmpty(req.Ref, "latest"))
}

// register is the single path from manifest bytes to the registry for
// runtime sources (API uploads, restores and kept ephemeral state).
func (s *Server) register(w http.ResponseWriter, id string, manifest []byte, source string) {
	a, err := plugin.Parse(manifest)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_manifest", err.Error(), "manifest")
		return
	}
	if a.ID != id {
		writeErr(w, http.StatusBadRequest, "id_mismatch", fmt.Sprintf("manifest id %q does not match path id %q", a.ID, id), "id")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	status, res, err := s.registerLocked(a, manifest, source)
	if err != nil {
		code := "registration_rejected"
		if errors.Is(err, plugin.ErrProtected) {
			code = "plugin_protected"
		}
		writeErr(w, status, code, err.Error(), "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(res)
}

func (s *Server) registerLocked(a plugin.App, manifest []byte, source string) (int, registrationResult, error) {
	snap := s.registry.Snapshot()
	existing, exists := snap.ByID(a.ID)
	if exists && existing.Tier != plugin.TierEphemeral {
		return http.StatusConflict, registrationResult{}, fmt.Errorf("%w: %s is %s", plugin.ErrProtected, a.ID, existing.Tier)
	}
	if err := s.checkRuntimePolicy(a); err != nil {
		return http.StatusUnprocessableEntity, registrationResult{}, err
	}
	caps := s.effectiveCapabilities(a)
	if caps.Sensitive() && !(exists && existing.Granted.Covers(caps)) {
		p := &Pending{Kind: pendingRegister, App: a, Manifest: manifest, Capabilities: caps, Source: source, Requested: time.Now()}
		if exists {
			p.Replaces = existing.App.Version
		}
		s.pending[a.ID] = p
		s.registryChangedLocked(snap.Version, a.ID, "pending", plugin.TierEphemeral)
		return http.StatusAccepted, registrationResult{RegistryVersion: snap.Version, Plugin: infoForPending(p), Message: "registration requests " + caps.String() + "; confirm it on the device to activate"}, nil
	}
	delete(s.pending, a.ID)
	e, next, err := s.activateLocked(a, manifest, caps, source)
	if err != nil {
		return http.StatusConflict, registrationResult{}, err
	}
	status := http.StatusCreated
	if exists {
		status = http.StatusOK
	}
	return status, registrationResult{RegistryVersion: next.Version, Plugin: s.infoForEntry(e)}, nil
}

// activateLocked stores and publishes an ephemeral entry. Jobs already running
// for a replaced version keep the definition they started with.
func (s *Server) activateLocked(a plugin.App, manifest []byte, granted plugin.Capabilities, source string) (plugin.Entry, *plugin.Snapshot, error) {
	path, err := s.opts.Store.Write(plugin.TierEphemeral, a.ID, manifest)
	if err != nil {
		return plugin.Entry{}, nil, err
	}
	e := plugin.Entry{App: a, Tier: plugin.TierEphemeral, Source: source + " -> " + path, Manifest: manifest, Granted: granted}
	prev, next, err := s.registry.Put(e)
	if err != nil {
		return plugin.Entry{}, nil, err
	}
	change := "added"
	if prev != nil {
		change = "replaced"
	}
	slog.Info("plugin registered", "id", a.ID, "version", a.Version, "tier", e.Tier, "change", change, "capabilities", granted.String(), "source", source)
	s.registryChangedLocked(next.Version, a.ID, change, e.Tier)
	return e, next, nil
}

func (s *Server) pluginDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pending[id]; ok && p.Kind == pendingRegister {
		if _, active := s.registry.Snapshot().ByID(id); !active {
			delete(s.pending, id)
			s.registryChangedLocked(s.registry.Snapshot().Version, id, "rejected", plugin.TierEphemeral)
			j(w, map[string]any{"pluginId": id, "state": "rejected"})
			return
		}
	}
	removed, next, err := s.removeLocked(id)
	if err != nil {
		status, code := http.StatusNotFound, "plugin_not_found"
		if errors.Is(err, plugin.ErrProtected) {
			status, code = http.StatusConflict, "plugin_protected"
		}
		writeErr(w, status, code, err.Error(), "id")
		return
	}
	j(w, map[string]any{"pluginId": id, "state": "removed", "tier": removed.Tier, "registryVersion": next.Version})
}

// removeLocked removes a runtime entry, archives its manifest, cancels its
// running jobs (its capability grant is revoked with it) and sends sessions
// that were showing it back home.
func (s *Server) removeLocked(id string) (plugin.Entry, *plugin.Snapshot, error) {
	removed, next, err := s.registry.Remove(id)
	if err != nil {
		return removed, next, err
	}
	delete(s.pending, id)
	if len(removed.Manifest) > 0 {
		if _, err := s.opts.Store.Archive(removed.App, removed.Manifest); err != nil {
			slog.Warn("archive removed plugin", "id", id, "error", err)
		}
	}
	if err := s.opts.Store.Delete(removed.Tier, id); err != nil {
		slog.Warn("delete removed plugin manifest", "id", id, "error", err)
	}
	for jobID, jb := range s.jobs {
		if jb.AppID == id && jb.State == "running" {
			s.cancelReasons[jobID] = "plugin removed"
			if c := s.cancels[jobID]; c != nil {
				c()
			}
		}
	}
	for _, ss := range s.sessionsStore {
		if ss.CurrentAppID == id && ss.SelectedJobID == "" {
			ss.CurrentAppID = "home"
		}
	}
	slog.Info("plugin removed", "id", id, "tier", removed.Tier)
	s.registryChangedLocked(next.Version, id, "removed", removed.Tier)
	return removed, next, nil
}

func (s *Server) pluginPromote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.registry.Snapshot().ByID(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "plugin not found", "id")
		return
	}
	if e.Tier != plugin.TierEphemeral {
		writeErr(w, http.StatusConflict, "not_ephemeral", "only ephemeral plugins can be promoted", "id")
		return
	}
	// Promotion makes a runtime registration survive restarts, so it always
	// needs its own device confirmation, even for apps without capabilities.
	p := &Pending{Kind: pendingPromote, App: e.App, Manifest: e.Manifest, Capabilities: e.Granted, Source: "promote:" + clientHost(r), Requested: time.Now()}
	s.pending[id] = p
	ver := s.registry.Snapshot().Version
	s.registryChangedLocked(ver, id, "pending", plugin.TierInstalled)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(registrationResult{RegistryVersion: ver, Plugin: infoForPending(p), Message: "promotion to installed must be confirmed on the device"})
}

// confirmLocked applies a pending registration or promotion. It is only
// reachable from a device confirmation, never from the registration API.
func (s *Server) confirmLocked(id string) error {
	p, ok := s.pending[id]
	if !ok {
		return fmt.Errorf("nothing pending for %s", id)
	}
	delete(s.pending, id)
	switch p.Kind {
	case pendingRegister:
		// Re-check policy against the current registry: run targets or
		// command roots may have changed while this was pending.
		if err := s.checkRuntimePolicy(p.App); err != nil {
			return err
		}
		caps := s.effectiveCapabilities(p.App)
		if !p.Capabilities.Covers(caps) {
			return fmt.Errorf("%s now needs %s; register it again", id, caps.String())
		}
		_, _, err := s.activateLocked(p.App, p.Manifest, caps, p.Source+" (confirmed on device)")
		return err
	case pendingPromote:
		path, err := s.opts.Store.Promote(id)
		if err != nil {
			return err
		}
		e, next, err := s.registry.Retier(id, plugin.TierInstalled, "promoted -> "+path)
		if err != nil {
			return err
		}
		slog.Info("plugin promoted", "id", id, "path", path)
		s.registryChangedLocked(next.Version, id, "promoted", e.Tier)
		return nil
	}
	return fmt.Errorf("unknown pending kind %q", p.Kind)
}

func (s *Server) rejectLocked(id string) error {
	if _, ok := s.pending[id]; !ok {
		return fmt.Errorf("nothing pending for %s", id)
	}
	delete(s.pending, id)
	s.registryChangedLocked(s.registry.Snapshot().Version, id, "rejected", "")
	return nil
}

// RestoreEphemeral re-registers manifests kept from a previous daemon run
// (-keep-ephemeral). Grants do not survive restarts, so sensitive ones become
// pending again.
func (s *Server) RestoreEphemeral(entries []plugin.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if status, _, err := s.registerLocked(e.App, e.Manifest, "state:kept"); err != nil {
			slog.Warn("kept ephemeral plugin not restored", "id", e.App.ID, "status", status, "error", err)
		}
	}
}

// AdoptInstalled activates installed entries loaded from the state
// directory. Installed apps were confirmed when they were promoted, so they
// keep their effective capabilities across restarts; they must still pass the
// runtime policy. Command apps are adopted before views that run them.
func (s *Server) AdoptInstalled(entries []plugin.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].App.Kind != plugin.KindViewApp && entries[j].App.Kind == plugin.KindViewApp
	})
	for _, e := range entries {
		if err := s.checkRuntimePolicy(e.App); err != nil {
			slog.Warn("installed plugin skipped", "id", e.App.ID, "error", err)
			continue
		}
		e.Tier, e.Granted = plugin.TierInstalled, s.effectiveCapabilities(e.App)
		e.Source = "state:" + e.Source
		if _, _, err := s.registry.Put(e); err != nil {
			slog.Warn("installed plugin skipped", "id", e.App.ID, "error", err)
		}
	}
}

func checkExecutableRoots(exe string, roots []string) error {
	if exe == "" {
		return nil
	}
	if len(roots) == 0 {
		return errors.New("runtime command apps are disabled: no -command-roots configured")
	}
	if !strings.Contains(exe, "/") {
		return fmt.Errorf("executable %q must be a path inside a command root, not a PATH lookup", exe)
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("executable %q: %w", exe, err)
	}
	for _, root := range roots {
		// Roots may be relative to the daemon's working directory (the
		// default is "fakecmds"); compare absolute, symlink-free paths.
		ar, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rr, err := filepath.EvalSymlinks(ar)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(rr, real); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if fi, err := os.Stat(real); err == nil && fi.Mode().IsRegular() {
				return nil
			}
		}
	}
	return fmt.Errorf("executable %q is outside the allowed command roots %v", exe, roots)
}

func (s *Server) registryChangedLocked(version uint64, id, change string, tier plugin.Tier) {
	for _, ss := range s.sessionsStore {
		onHome := (ss.CurrentAppID == "" || ss.CurrentAppID == "home") && ss.SelectedJobID == ""
		if onHome || ss.CurrentAppID == id {
			ss.Revision++
			s.viewChangedLocked(ss)
		}
	}
	s.appendEventLocked(Event{Type: "registry.changed", Registry: &RegistryChange{Version: version, PluginID: id, Change: change, Tier: tier}, Time: time.Now().Format(time.RFC3339)})
}

func (s *Server) infoForEntry(e plugin.Entry) pluginInfo {
	g := e.Granted
	info := pluginInfo{ID: e.App.ID, Title: e.App.Title, Version: e.App.Version, Kind: e.App.Kind, Tier: e.Tier, Source: e.Source, State: "active", Capabilities: e.App.Capabilities, Granted: &g, Owner: e.App.Owner}
	if !e.Updated.IsZero() {
		u := e.Updated
		info.Updated = &u
	}
	if p, ok := s.pending[e.App.ID]; ok {
		info.Pending = p
	}
	return info
}

func infoForPending(p *Pending) pluginInfo {
	tier := plugin.TierEphemeral
	if p.Kind == pendingPromote {
		tier = plugin.TierInstalled
	}
	return pluginInfo{ID: p.App.ID, Title: p.App.Title, Version: p.App.Version, Kind: p.App.Kind, Tier: tier, Source: p.Source, State: "pending", Capabilities: p.Capabilities, Owner: p.App.Owner, Pending: p}
}

func (s *Server) sandboxName() string {
	if s.opts.Sandbox == nil {
		return "none"
	}
	return s.opts.Sandbox.Name()
}

// runOptionsFor picks the sandbox policy for a job from the entry's tier and
// granted capabilities.
func (s *Server) runOptionsFor(e plugin.Entry) (sandbox.Sandbox, sandbox.Policy) {
	if e.Tier == plugin.TierSystem && !s.opts.SandboxSystem {
		return nil, sandbox.Policy{}
	}
	sb := s.opts.Sandbox
	if sb == nil {
		sb = sandbox.None{Reason: "no sandbox configured"}
	}
	wd, _ := os.Getwd()
	p := sandbox.Policy{Network: e.Granted.Network, Devices: e.Granted.Devices, WorkDir: wd}
	for _, r := range s.opts.CommandRoots {
		if abs, err := filepath.Abs(r); err == nil {
			p.ReadOnly = append(p.ReadOnly, abs)
		}
	}
	return sb, p
}

func clientHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
