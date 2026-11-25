package plugin

import (
	"errors"
	"os"
	"strings"
	"testing"
)

const pingManifest = `apiVersion: flipctl.app/v1alpha1
kind: CommandApp
metadata:
  id: network.example
  title: Example
  version: 0.1.0
form:
  - id: target
    label: Target
    type: string
    required: true
    pattern: '^[a-z.]+$'
execution:
  executable: ./fakecmds/fake-ping
  args: ["-c", "1", "{{target}}"]
  timeoutSeconds: 5
parser: ping
`

func TestCheckedInCatalogsPassSharedValidation(t *testing.T) {
	for _, dir := range []string{"../../plugins", "../../plugins-real"} {
		apps, err := LoadDir(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if len(apps) != 2 {
			t.Fatalf("%s: loaded %d apps", dir, len(apps))
		}
	}
}

func TestParseAcceptsYAMLAndEquivalentJSON(t *testing.T) {
	a, err := Parse([]byte(pingManifest))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "network.example" || a.Command.Parser != "ping" || a.Availability["state"] != "available" {
		t.Fatalf("unexpected normalized app %#v", a)
	}
	j := `{"apiVersion":"flipctl.app/v1alpha1","kind":"CommandApp","metadata":{"id":"network.example","title":"Example"},"execution":{"executable":"./fakecmds/fake-ping"}}`
	if _, err := Parse([]byte(j)); err != nil {
		t.Fatalf("json manifest rejected: %v", err)
	}
}

func TestParseRejectsInvalidUploadedManifests(t *testing.T) {
	cases := map[string]struct {
		edit   func(string) string
		schema bool
	}{
		"unknown root key": {func(s string) string { return s + "hooks: {pre: rm}\n" }, true},
		"unknown field key": {func(s string) string {
			return strings.Replace(s, "    required: true\n", "    required: true\n    html: '<b>'\n", 1)
		}, true},
		"bad id":              {func(s string) string { return strings.Replace(s, "network.example", "../etc/passwd", 1) }, true},
		"wrong api version":   {func(s string) string { return strings.Replace(s, "flipctl.app/v1alpha1", "flipctl.app/v2", 1) }, true},
		"missing executable":  {func(s string) string { return strings.Replace(s, "  executable: ./fakecmds/fake-ping\n", "", 1) }, true},
		"shell metacharacter": {func(s string) string { return strings.Replace(s, `"{{target}}"`, `"{{target}};reboot"`, 1) }, false},
		"invalid regex":       {func(s string) string { return strings.Replace(s, `'^[a-z.]+$'`, `'^[a-z'`, 1) }, false},
	}
	for name, tc := range cases {
		_, err := Parse([]byte(tc.edit(pingManifest)))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		var se *SchemaError
		if errors.As(err, &se) != tc.schema {
			t.Errorf("%s: schema error=%v, want %v (%v)", name, errors.As(err, &se), tc.schema, err)
		}
	}
}

func TestParseRejectsOversizedManifest(t *testing.T) {
	big := pingManifest + "# " + strings.Repeat("x", MaxManifestBytes) + "\n"
	if _, err := Parse([]byte(big)); err == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func TestCapabilitiesAreDeclaredAndValidated(t *testing.T) {
	m := pingManifest + "capabilities:\n  network: true\n  devices: [/dev/ttyLoRa0]\n  rf_transmit: false\n"
	a, err := Parse([]byte(m))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Capabilities.Network || len(a.Capabilities.Devices) != 1 || !a.Capabilities.Sensitive() {
		t.Fatalf("capabilities not parsed: %#v", a.Capabilities)
	}
	for _, bad := range []string{
		"capabilities:\n  devices: [/etc/shadow]\n",
		"capabilities:\n  devices: [/dev/../etc/shadow]\n",
		"capabilities:\n  agent: true\n",
		"capabilities:\n  root: true\n",
	} {
		if _, err := Parse([]byte(pingManifest + bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if (Capabilities{}).Sensitive() {
		t.Fatal("empty capabilities must not be sensitive")
	}
}

func TestCapabilitiesCoversDetectsEscalation(t *testing.T) {
	granted := Capabilities{Devices: []string{"/dev/ttyLoRa0"}}
	if !granted.Covers(Capabilities{Devices: []string{"/dev/ttyLoRa0"}}) {
		t.Fatal("same grant must be covered")
	}
	for _, more := range []Capabilities{{Network: true}, {RFTransmit: true}, {Agent: true}, {Devices: []string{"/dev/ttyUSB0"}}} {
		if granted.Covers(more) {
			t.Fatalf("escalation %v not detected", more)
		}
	}
	u := granted.Union(Capabilities{Network: true, Devices: []string{"/dev/ttyLoRa0", "/dev/a"}})
	if !u.Network || len(u.Devices) != 2 {
		t.Fatalf("bad union %#v", u)
	}
}

const calibrationView = `apiVersion: flipctl.app/v1alpha1
kind: ViewApp
metadata: {id: assistant.c12.lora-cal, title: LoRa antenna calibration, version: 0.1.0}
owner: {plugin: assistant, conversation: c-12}
form:
  - {id: count, label: Packets, type: int, default: "20", min: 5, max: 200}
actions:
  - id: capture
    label: Capture packets
    run: {plugin: field.lora-rx, args: {count: "{{count}}"}}
  - id: advise
    label: Which angle is best?
    ask: {prompt: "Which antenna angle gives the best link?", context: [lastJob]}
display:
  - {from: lastJob, as: table, columns: [seq, rssi, snr]}
`

func TestViewAppComposesActionsWithoutExecutables(t *testing.T) {
	a, err := Parse([]byte(calibrationView))
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != KindViewApp || !a.UsesAssistant() || strings.Join(a.RunTargets(), ",") != "field.lora-rx" {
		t.Fatalf("unexpected view app %#v", a)
	}
	if act, _ := a.ActionByID("advise"); act.Mode() != "ask" {
		t.Fatalf("advise mode %q", act.Mode())
	}
	bad := map[string]string{
		"executable":    strings.Replace(calibrationView, "owner:", "execution: {executable: ./fakecmds/fake-ping}\nowner:", 1),
		"declared caps": calibrationView + "capabilities: {network: true}\n",
		"two bindings": strings.Replace(calibrationView, `    run: {plugin: field.lora-rx, args: {count: "{{count}}"}}`, `    run: {plugin: field.lora-rx}
    ask: {prompt: hi}`, 1),
		"no binding":        strings.Replace(calibrationView, `    run: {plugin: field.lora-rx, args: {count: "{{count}}"}}`+"\n", "", 1),
		"shell in args":     strings.Replace(calibrationView, `"{{count}}"`, `"{{count}};reboot"`, 1),
		"self run":          strings.Replace(calibrationView, "plugin: field.lora-rx", "plugin: assistant.c12.lora-cal", 1),
		"actions on cmdapp": pingManifest + "actions: [{id: x, label: x, ask: {prompt: hi}}]\n",
	}
	for name, m := range bad {
		if _, err := Parse([]byte(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRuntimeRegistrationExamplesAreValid(t *testing.T) {
	for _, f := range []string{"lora-rx.yaml", "lora-rx-v2.yaml", "lora-calibration.yaml"} {
		b, err := os.ReadFile("../../examples/runtime-registration/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(b); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestPluginGuideManifestExamplesParse(t *testing.T) {
	b, err := os.ReadFile("../../docs/plugin.md")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(b), "```yaml\n")
	n := 0
	for _, p := range parts[1:] {
		block := p[:strings.Index(p, "```")]
		if !strings.HasPrefix(block, "apiVersion:") {
			continue
		}
		n++
		if _, err := Parse([]byte(block)); err != nil {
			t.Errorf("docs/plugin.md example %d: %v", n, err)
		}
	}
	if n < 2 {
		t.Fatalf("found %d manifest examples", n)
	}
}
