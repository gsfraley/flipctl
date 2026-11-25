package api

import (
	"fmt"
	"strings"

	"github.com/flipperdevices/flipctl/internal/plugin"
	"github.com/flipperdevices/flipctl/internal/viewdoc"
)

// homeBlocksLocked projects the registry into the home view. System and
// installed apps keep the original "apps" block; runtime-registered apps and
// pending confirmations get their own clearly labelled blocks so every
// renderer marks them without any new ViewDocument vocabulary.
func (s *Server) homeBlocksLocked() ([]viewdoc.Block, []viewdoc.Action) {
	snap := s.registry.Snapshot()
	trusted, ephemeral := []viewdoc.ListItem{}, []viewdoc.ListItem{}
	for _, e := range snap.Entries {
		a := e.App
		item := viewdoc.ListItem{Label: a.Title, Value: a.ID, Description: a.Summary, Action: &viewdoc.Action{ID: "app.open", Label: "Open " + a.Title, Enabled: true, Params: map[string]string{"appId": a.ID}}}
		switch e.Tier {
		case plugin.TierEphemeral:
			item.Label = "[ephemeral] " + a.Title
			item.Description = joinNonEmpty(" · ", "v"+firstNonEmpty(a.Version, "0"), "uses "+e.Granted.String(), a.Summary)
			ephemeral = append(ephemeral, item)
		case plugin.TierInstalled:
			item.Description = joinNonEmpty(" · ", "installed", a.Summary)
			trusted = append(trusted, item)
		default:
			trusted = append(trusted, item)
		}
	}
	blocks := []viewdoc.Block{{ID: "apps", Kind: "list", Title: "Apps", Items: trusted}}
	var actions []viewdoc.Action
	if len(ephemeral) > 0 {
		blocks = append(blocks,
			viewdoc.Block{ID: "ephemeral-notice", Kind: "notice", Severity: "warning", Text: "Runtime-registered apps are ephemeral: they were added while FlipCTL was running, are not part of the system image, " + s.sandboxPhrase() + ", and are cleared when the daemon restarts."},
			viewdoc.Block{ID: "ephemeral-apps", Kind: "list", Title: "Runtime-registered (ephemeral)", Items: ephemeral})
	}
	if len(s.pending) > 0 {
		items := []viewdoc.ListItem{}
		for _, id := range sortedKeys(s.pending) {
			p := s.pending[id]
			verb := "Register"
			if p.Kind == pendingPromote {
				verb = "Install permanently"
			}
			desc := joinNonEmpty(" · ", verb, "v"+firstNonEmpty(p.App.Version, "0"), "requests "+p.Capabilities.String())
			if p.Replaces != "" {
				desc += " · replaces v" + p.Replaces
			}
			items = append(items, viewdoc.ListItem{Label: p.App.Title, Value: id, Description: desc, Action: &viewdoc.Action{ID: "device.confirm_registration", Label: "Confirm " + p.App.Title, Enabled: s.opts.EmulatedDeviceConfirm, Role: "primary", Params: map[string]string{"pluginId": id}}})
			actions = append(actions, viewdoc.Action{ID: "device.reject_registration", Label: "Reject " + p.App.Title, Enabled: true, Role: "destructive", Params: map[string]string{"pluginId": id}})
		}
		text := "Confirm with the device button. Nothing was activated yet."
		if s.opts.EmulatedDeviceConfirm {
			text = "Confirm with the device button. In this emulated build, the Confirm action stands in for the physical button press."
		}
		blocks = append(blocks,
			viewdoc.Block{ID: "pending-notice", Kind: "notice", Severity: "warning", Text: text},
			viewdoc.Block{ID: "pending-registrations", Kind: "list", Title: "Waiting for device confirmation", Items: items})
	}
	return blocks, actions
}

// sandboxPhrase states how runtime commands run, without overclaiming when the
// daemon fell back to the unsandboxed runner.
func (s *Server) sandboxPhrase() string {
	if s.sandboxName() == "none" {
		return "run WITHOUT a sandbox on this build"
	}
	return "run sandboxed (" + s.sandboxName() + ")"
}

func ephemeralNotice(e plugin.Entry) viewdoc.Block {
	return viewdoc.Block{ID: "ephemeral-notice", Kind: "notice", Severity: "warning", Text: fmt.Sprintf("Ephemeral app v%s registered at runtime (%s). Uses: %s.", firstNonEmpty(e.App.Version, "0"), e.Source, e.Granted.String())}
}

// handleDeviceActionLocked handles the emulated device button. On hardware
// this would be driven by the MCU input adapter instead of a session action.
func (s *Server) handleDeviceActionLocked(ss *Session, actionID, pluginID string) error {
	if pluginID == "" {
		return fmt.Errorf("params.pluginId is required")
	}
	var err error
	switch actionID {
	case "device.confirm_registration":
		if !s.opts.EmulatedDeviceConfirm {
			return fmt.Errorf("device confirmation is not available as a session action on this build")
		}
		err = s.confirmLocked(pluginID)
		if err != nil {
			s.registryChangedLocked(s.registry.Snapshot().Version, pluginID, "rejected", "")
		}
	case "device.reject_registration":
		err = s.rejectLocked(pluginID)
	default:
		return fmt.Errorf("unsupported device action %q", actionID)
	}
	ss.Revision++
	s.viewChangedLocked(ss)
	return err
}

func joinNonEmpty(sep string, parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" && p != "v" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
