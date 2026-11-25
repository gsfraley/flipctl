package plugin

import (
	"errors"
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
