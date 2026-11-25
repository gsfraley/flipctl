# Plugin guide

FlipCTL command plugins are declarative YAML descriptors loaded by `flipctld` from its `-plugins` catalog at startup or registered at runtime through `/api/v1/plugins` (see [Runtime registration](runtime-registration.md)). Both paths use the same parser and [`schemas/plugin-manifest.json`](../schemas/plugin-manifest.json). A plugin defines an app, its typed form fields, safe command mapping, parser choice, and policy hints. Plugins do **not** define HTML, TUI widgets, Canvas drawing code, frontend routes, or renderer-specific templates.

## Canonical manifest

```yaml
apiVersion: flipctl.app/v1alpha1
kind: CommandApp
metadata:
  id: network.example
  title: Example probe
  summary: Deterministic example command app
  category: Network
  version: 0.1.0
form:
  - id: target
    label: Target
    type: string
    required: true
    default: 127.0.0.1
    pattern: '^[A-Za-z0-9_.:-]+$'
  - id: count
    label: Count
    type: int
    default: "1"
    min: 1
    max: 5
execution:
  executable: ./fakecmds/fake-ping
  args: ["-c", "{{count}}", "{{target}}"]
  timeoutSeconds: 5
parser: ping
policy:
  fakeDefault: true
  network: icmp
```

The example matches the same normalized shape used by `plugins/ping.yaml` and passes `internal/plugin.Parse`: schema validation against `schemas/plugin-manifest.json`, strict decoding (unknown keys are rejected), normalization and `internal/plugin.Validate`.

## Typed fields

Each `form` field has:

- `id` — safe identifier used for template substitution and session form state.
- `label` — renderer-neutral label.
- `type` — currently `string` or `int` in the checked-in plugins.
- `required` — whether the daemon should require a value.
- `default` — string default shown in the form.
- `pattern` — optional regular expression for string validation hints.
- `min` / `max` — optional integer bounds.

## Command mapping

`execution.executable` names the binary to run and `execution.args` is an argument vector. FlipCTL uses direct exec-style invocation, not a shell. Templates such as `{{target}}` are replaced from validated form values. `timeoutSeconds` bounds the process lifetime.

Unsafe shell metacharacters in executable or argument templates fail validation. Keep commands deterministic in default plugins.

## Capabilities

`capabilities` declares the host effects an app needs:

```yaml
capabilities:
  network: true            # share the host network namespace
  devices: [/dev/ttyLoRa0] # device nodes the command may open
  rf_transmit: false       # can key a transmitter
```

Capabilities decide the sandbox policy for an app's commands and whether a runtime registration has to be confirmed on the device. Omitting the section means no network, no extra devices and no transmission when the command runs sandboxed. A fourth capability, `agent`, is computed by the daemon for apps that send data to an assistant and cannot be declared.

## Parser responsibility

`parser` selects daemon-side parsing for tool output, for example `ping` or `nmap`. Parsers may understand tool-specific output, but they are internal to the daemon. Parser results must be projected into generic `ViewDocument` blocks before reaching Web, TUI, or Canvas.

## Generic presentation responsibility

The daemon is responsible for translating plugin/app/job state into `text`, `notice`, `list`, `form`, `key_value`, `table`, `log`, and `progress` blocks. Frontends render those generic blocks and submit semantic session actions. A plugin must never define HTML/TUI/Canvas UI.

## ViewApps

`kind: ViewApp` describes a view with no executable of its own; it is the shape generated "sub-plugins" take. Each action binds exactly one of `run` (start a registered command app with arguments templated from the view's form), `ask`, `revise` or `act` (send the view's state to the configured assistant provider). `display` bindings project the last job's JSON-lines output into generic `table` or `log` blocks.

```yaml
apiVersion: flipctl.app/v1alpha1
kind: ViewApp
metadata: {id: assistant.c-demo.lora-calibration, title: LoRa antenna calibration, version: 0.1.0}
owner: {plugin: assistant, conversation: c-demo}
form:
  - {id: packets, label: Packets per capture, type: int, default: "10", min: 1, max: 100}
actions:
  - id: capture
    label: Capture packets
    run: {plugin: field.lora-rx, args: {count: "{{packets}}"}}
  - id: advise
    label: Which reading was best?
    ask: {prompt: "Which packet had the best link?", context: [lastJob]}
display:
  - {from: lastJob, as: table, columns: [seq, rssi, snr, lat, lon], limit: 8}
```

A ViewApp cannot declare capabilities: the daemon computes them from its run targets' grants, plus `agent` if any action uses the assistant.

## Trust tiers and runtime registration

| Tier | Where it comes from | Runtime changes |
| --- | --- | --- |
| `system` | `-plugins` catalog | none |
| `installed` | promoted from `ephemeral` after a device confirmation | remove |
| `ephemeral` | `PUT /api/v1/plugins/{id}` | replace, remove, promote |

Runtime-registered command apps must point at an executable inside a `-command-roots` directory, and their commands run sandboxed. Registrations with sensitive capabilities wait for a device confirmation. See [Runtime registration](runtime-registration.md) for the API, the confirmation gate and the security model.

## Fake versus Linux command catalog

- `plugins/` is the default deterministic catalog. It uses `fakecmds/fake-ping` and `fakecmds/fake-nmap` so local tests and screenshots are reproducible without host tools or network access.
- `plugins-real/` is the opt-in Linux tools catalog for Docker `real-tools` demos. It invokes real `ping` and `nmap` inside the container and is not required by `make check`.

## Adding a command app

1. Add a YAML manifest under `plugins/` for deterministic default behavior, or under `plugins-real/` only for opt-in real tools.
2. Use `apiVersion: flipctl.app/v1alpha1` and `kind: CommandApp`.
3. Pick a stable `metadata.id` such as `network.trace`.
4. Define typed `form` fields with defaults and validation hints.
5. Map `execution.executable` and `execution.args` without shell syntax.
6. Select or add a parser if raw output needs structured treatment.
7. Project results through the existing ViewDocument model; do not add app-specific renderer fields.
8. Add tests for loading/validation, runner argument construction, parser behavior, and canonical view projection.

Useful checks:

```bash
go test ./internal/plugin ./internal/runner ./internal/api -count=1
make renderer-contract
```

## Future plugin tiers

The MVP plugin tier is declarative command wrappers and ViewApps composed from them, registered either from the catalog or at runtime. Native subprocess plugins and WebAssembly Component Model plugins are future tiers. They must preserve the same daemon-owned sessions, jobs, events, and renderer-neutral `ViewDocument` contract rather than introducing frontend-specific UI definitions.
