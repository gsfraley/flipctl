package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Field struct {
	ID       string `json:"id" yaml:"id"`
	Label    string `json:"label" yaml:"label"`
	Type     string `json:"type" yaml:"type"`
	Default  string `json:"default" yaml:"default"`
	Required bool   `json:"required" yaml:"required"`
	Pattern  string `json:"pattern" yaml:"pattern"`
	Min      int    `json:"min" yaml:"min"`
	Max      int    `json:"max" yaml:"max"`
}
type Command struct {
	Executable     string   `json:"executable" yaml:"executable"`
	Args           []string `json:"args" yaml:"args"`
	TimeoutSeconds int      `json:"timeoutSeconds" yaml:"timeoutSeconds"`
	Parser         string   `json:"parser" yaml:"parser"`
}
type Metadata struct {
	ID       string `json:"id" yaml:"id"`
	Title    string `json:"title" yaml:"title"`
	Summary  string `json:"summary" yaml:"summary"`
	Category string `json:"category" yaml:"category"`
	Version  string `json:"version" yaml:"version"`
}
type App struct {
	APIVersion   string            `json:"apiVersion" yaml:"apiVersion"`
	Kind         string            `json:"kind,omitempty" yaml:"kind"`
	ID           string            `json:"id" yaml:"id"`
	Title        string            `json:"title" yaml:"title"`
	Summary      string            `json:"summary" yaml:"summary"`
	Category     string            `json:"category" yaml:"category"`
	Version      string            `json:"version" yaml:"version"`
	Metadata     Metadata          `json:"metadata,omitempty" yaml:"metadata"`
	Fields       []Field           `json:"fields" yaml:"fields"`
	Form         []Field           `json:"form,omitempty" yaml:"form"`
	Command      Command           `json:"command" yaml:"command"`
	Execution    Command           `json:"execution,omitempty" yaml:"execution"`
	Parser       string            `json:"parser,omitempty" yaml:"parser"`
	Policy       map[string]any    `json:"policy,omitempty" yaml:"policy"`
	Availability map[string]string `json:"availability" yaml:"availability"`
	Capabilities Capabilities      `json:"capabilities" yaml:"capabilities"`
	// Owner records which app and conversation registered this app, so
	// archived registrations can be found and restored from that context.
	Owner *Owner `json:"owner,omitempty" yaml:"owner"`
	// Actions and Display are only used by kind ViewApp.
	Actions []ViewAction     `json:"actions,omitempty" yaml:"actions"`
	Display []DisplayBinding `json:"display,omitempty" yaml:"display"`
}

type Owner struct {
	Plugin       string `json:"plugin" yaml:"plugin"`
	Conversation string `json:"conversation,omitempty" yaml:"conversation"`
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

func LoadDir(dir string) ([]App, error) {
	var out []App
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e != nil || d.IsDir() || !(strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) {
			return e
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		a, e := Parse(b)
		if e != nil {
			return fmt.Errorf("%s: %w", p, e)
		}
		out = append(out, a)
		return nil
	})
	return out, err
}
func normalize(a *App) {
	if a.ID == "" {
		a.ID = a.Metadata.ID
	}
	if a.Title == "" {
		a.Title = a.Metadata.Title
	}
	if a.Summary == "" {
		a.Summary = a.Metadata.Summary
	}
	if a.Category == "" {
		a.Category = a.Metadata.Category
	}
	if a.Version == "" {
		a.Version = a.Metadata.Version
	}
	if len(a.Fields) == 0 && len(a.Form) > 0 {
		a.Fields = a.Form
	}
	if a.Command.Executable == "" && a.Execution.Executable != "" {
		a.Command = a.Execution
	}
	if a.Command.Parser == "" && a.Parser != "" {
		a.Command.Parser = a.Parser
	}
	if a.APIVersion == "" {
		a.APIVersion = "flipctl.app/v1alpha1"
	}
	if a.Kind == "" {
		a.Kind = "CommandApp"
	}
}
func Validate(a App) error {
	if a.ID == "" || !safeID.MatchString(a.ID) {
		return errors.New("invalid app id")
	}
	if err := validateViewApp(a); err != nil {
		return err
	}
	if a.Kind != KindViewApp && a.Command.Executable == "" {
		return errors.New("missing executable")
	}
	if strings.ContainsAny(a.Command.Executable, ";&|$`\n") {
		return errors.New("unsafe executable")
	}
	for _, x := range a.Command.Args {
		if strings.ContainsAny(x, ";&|$`\n") {
			return fmt.Errorf("unsafe arg template %q", x)
		}
	}
	if err := validateCapabilities(a.Capabilities); err != nil {
		return err
	}
	for _, f := range a.Fields {
		if f.ID == "" || !safeID.MatchString(f.ID) {
			return fmt.Errorf("invalid field id %q", f.ID)
		}
		if f.Pattern != "" {
			if _, err := regexp.Compile(f.Pattern); err != nil {
				return fmt.Errorf("field %s: invalid pattern: %w", f.ID, err)
			}
		}
	}
	return nil
}
func ByID(apps []App, id string) (App, bool) {
	for _, a := range apps {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}
