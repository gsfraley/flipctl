package plugin

import (
	"errors"
	"fmt"
	"strings"
)

// Manifest kinds.
const (
	KindCommandApp = "CommandApp"
	// KindViewApp is a declarative view with no executable of its own. Its
	// actions either run an already-registered CommandApp ("run") or send the
	// view's state to the configured assistant provider ("ask", "revise",
	// "act"). This is the shape generated sub-plugins take.
	KindViewApp = "ViewApp"
)

// ViewAction is one button of a ViewApp. Exactly one binding is set.
type ViewAction struct {
	ID     string      `json:"id" yaml:"id"`
	Label  string      `json:"label" yaml:"label"`
	Run    *RunBinding `json:"run,omitempty" yaml:"run"`
	Ask    *Prompt     `json:"ask,omitempty" yaml:"ask"`
	Revise *Prompt     `json:"revise,omitempty" yaml:"revise"`
	Act    *Prompt     `json:"act,omitempty" yaml:"act"`
}

// RunBinding starts a job of another registered CommandApp. Args map the
// target's form field ids to templates over this view's form fields.
type RunBinding struct {
	Plugin string            `json:"plugin" yaml:"plugin"`
	Args   map[string]string `json:"args,omitempty" yaml:"args"`
}

// Prompt is a templated request to the assistant provider.
type Prompt struct {
	Prompt  string   `json:"prompt" yaml:"prompt"`
	Context []string `json:"context,omitempty" yaml:"context"`
}

// DisplayBinding projects data into a generic ViewDocument block.
type DisplayBinding struct {
	From    string   `json:"from" yaml:"from"`
	As      string   `json:"as" yaml:"as"`
	Title   string   `json:"title,omitempty" yaml:"title"`
	Columns []string `json:"columns,omitempty" yaml:"columns"`
	Limit   int      `json:"limit,omitempty" yaml:"limit"`
}

// Mode reports which binding a ViewAction uses.
func (v ViewAction) Mode() string {
	switch {
	case v.Run != nil:
		return "run"
	case v.Ask != nil:
		return "ask"
	case v.Revise != nil:
		return "revise"
	case v.Act != nil:
		return "act"
	}
	return ""
}

// Prompt returns the assistant prompt for ask/revise/act actions.
func (v ViewAction) PromptBinding() *Prompt {
	switch {
	case v.Ask != nil:
		return v.Ask
	case v.Revise != nil:
		return v.Revise
	}
	return v.Act
}

// UsesAssistant reports whether any action sends data to the assistant.
func (a App) UsesAssistant() bool {
	for _, act := range a.Actions {
		if m := act.Mode(); m == "ask" || m == "revise" || m == "act" {
			return true
		}
	}
	return false
}

// RunTargets lists the CommandApps this ViewApp can start.
func (a App) RunTargets() []string {
	var out []string
	seen := map[string]bool{}
	for _, act := range a.Actions {
		if act.Run != nil && !seen[act.Run.Plugin] {
			seen[act.Run.Plugin] = true
			out = append(out, act.Run.Plugin)
		}
	}
	return out
}

// ActionByID returns the ViewAction with id.
func (a App) ActionByID(id string) (ViewAction, bool) {
	for _, act := range a.Actions {
		if act.ID == id {
			return act, true
		}
	}
	return ViewAction{}, false
}

func validateViewApp(a App) error {
	if a.Kind != KindViewApp {
		if len(a.Actions) > 0 || len(a.Display) > 0 {
			return errors.New("actions and display are only valid for kind ViewApp")
		}
		return nil
	}
	if a.Command.Executable != "" {
		return errors.New("a ViewApp cannot declare an executable; bind actions to registered apps instead")
	}
	if a.Capabilities.Network || a.Capabilities.RFTransmit || len(a.Capabilities.Devices) > 0 {
		return errors.New("a ViewApp's capabilities are computed from its run targets and cannot be declared")
	}
	if len(a.Actions) == 0 {
		return errors.New("a ViewApp needs at least one action")
	}
	ids := map[string]bool{}
	for _, act := range a.Actions {
		if !safeID.MatchString(act.ID) || ids[act.ID] {
			return fmt.Errorf("invalid or duplicate action id %q", act.ID)
		}
		ids[act.ID] = true
		n := 0
		for _, set := range []bool{act.Run != nil, act.Ask != nil, act.Revise != nil, act.Act != nil} {
			if set {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("action %s must bind exactly one of run, ask, revise or act", act.ID)
		}
		if act.Run != nil {
			if !safeID.MatchString(act.Run.Plugin) || act.Run.Plugin == a.ID {
				return fmt.Errorf("action %s: invalid run target %q", act.ID, act.Run.Plugin)
			}
			for k, v := range act.Run.Args {
				if !safeID.MatchString(k) || strings.ContainsAny(v, ";&|$`\n") {
					return fmt.Errorf("action %s: unsafe run arg %q", act.ID, k)
				}
			}
		}
		if p := act.PromptBinding(); act.Run == nil && strings.TrimSpace(p.Prompt) == "" {
			return fmt.Errorf("action %s: empty prompt", act.ID)
		}
	}
	return nil
}
