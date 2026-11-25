# Runtime registration of declared, sandboxed plugins

Status: proposal. Builds on the architecture in [PR #4](https://github.com/flipperdevices/flipctl/pull/4); see [ADR-0005](adr/0005-runtime-plugin-registration.md).

This document proposes letting `flipctld` accept new plugins **while it runs**, from a person, a script, or an assistant (an AI agent), and making them appear on every frontend immediately, without weakening the core's authority over what runs on the device.

## Motivation: a roaming field instrument

Flipper One is a portable instrument that people take into the field: to survey a LoRa link, find a rogue access point, calibrate an antenna, log sensor readings along a walk. The tool you need often does not exist yet when you leave, and the person holding the device is the one who knows what it should be.

Three ways that tool gets made on the spot:

- **By hand**: someone writes a ten-line manifest that wraps a command they already have.
- **By script**: a field kit pushes a set of tools for today's job and removes them afterwards.
- **By an assistant**: "make me a view that captures LoRa packets while I rotate the antenna and tells me which angle was best." An assistant running on the device (through an `AssistantApp`, see below), or an external agent driving this API (for example an MCP server wrapping it), writes the manifest and registers it.

In every case the result should look and behave like any other app on the Web UI, the TUI and the 256×144 display, and it should disappear when it is no longer wanted. Today the only way to add an app is to edit `plugins/` and restart the daemon.

## What stays the same

The proposal deliberately changes **when** plugins are registered, not **what** a plugin is:

- The extension point is the same declarative manifest (`flipctl.app/v1alpha1`), validated by the same code.
- Apps still never ship UI code. The daemon projects them into the same renderer-neutral `ViewDocument` blocks, so no renderer needed new vocabulary.
- The daemon stays the only authority over the registry, sessions, jobs and process execution.

## Design

### Versioned registry

`internal/plugin.Registry` holds the active apps as immutable, versioned snapshots behind an atomic pointer. Writers are serialized; readers (projections, job starts, listings) take one snapshot and never see a half-applied change or take a lock. Every change bumps `registryVersion`.

### Trust tiers

| Tier | Source | Changeable at runtime | Survives restart |
| --- | --- | --- | --- |
| `system` | `-plugins` catalog shipped with the image | No | Yes |
| `installed` | promoted from `ephemeral` (device-confirmed) | Can be removed | Yes (`<state-dir>/installed`) |
| `ephemeral` | registered through the API | Replace / remove / promote | No: archived at startup unless `-keep-ephemeral` |

An ephemeral app can never shadow an installed or system app. Ephemeral apps are visibly marked on every frontend: the home view lists them in a separate **Runtime-registered (ephemeral)** list with a warning notice, their items are prefixed `[ephemeral]`, and their own views carry a notice with version, source and granted capabilities. All of this uses existing `list`/`notice` blocks.

### Registration API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/v1/plugins` | Every app with `tier`, `source`, `state` (`active`/`pending`), declared and granted capabilities; `?archived=1` adds restorable manifests |
| `GET` | `/api/v1/plugins/{id}` | One app and its manifest |
| `PUT` | `/api/v1/plugins/{id}` | Register or replace (YAML or JSON body, max 64 KiB) |
| `DELETE` | `/api/v1/plugins/{id}` | Remove, or reject a pending registration |
| `POST` | `/api/v1/plugins/{id}/promote` | Request promotion to `installed` |
| `POST` | `/api/v1/plugins/{id}/restore` | Re-register an archived manifest |
| `GET` | `/api/v1/schema/plugin-manifest.json` | The manifest schema, for authors and agents |

Responses: `201` registered, `200` replaced, `202` pending device confirmation, `403` not local or cross-origin, `409` protected id, `422` invalid manifest or policy violation, `503` registration disabled (no state directory). The full contract is in [`api/openapi.yaml`](../api/openapi.yaml).

### One validation path

Uploaded manifests go through `plugin.Parse`, which `LoadDir` now also uses for on-disk catalogs:

1. Parse YAML/JSON (size-limited).
2. Validate against [`schemas/plugin-manifest.json`](../schemas/plugin-manifest.json) (JSON Schema 2020-12, the same dialect and library as `view-document.json`).
3. Strict decode (unknown keys rejected), normalize, then `plugin.Validate` for rules a schema cannot express (shell metacharacters, compilable patterns, clean `/dev` paths, ViewApp binding rules).

Runtime sources add one policy step: command executables must resolve, after symlinks, to a regular file inside a `-command-roots` directory (no `PATH` lookups, no `..` escapes), and ViewApp run targets must be registered command apps.

### Capabilities and the confirmation gate

Manifests declare the host effects they need:

```yaml
capabilities:
  network: true          # share the network namespace
  devices: [/dev/ttyLoRa0]
  rf_transmit: false     # can key a transmitter
```

A fourth capability, `agent` (the app sends on-device data to the assistant provider), is computed by the daemon and cannot be declared.

If an app's effective capabilities are sensitive, `PUT` does **not** activate it. It returns `202` and the registration waits for a **local device action**: on hardware, a physical button press reported by the MCU input adapter; in this emulated build, a `device.confirm_registration` session action (enabled with `-emulated-device-confirm`) that the TUI and Web UI show as a **Confirm** button. No registration API call can confirm, and re-uploading stays pending.

- Replacing an app with the same or fewer capabilities applies immediately; adding a capability needs a new confirmation, and the confirmed version stays active meanwhile.
- Promotion to `installed` always needs a confirmation, even without capabilities.
- Restores and kept ephemeral apps are fresh registrations: grants never survive a restart or an archive.

### ViewApps: generated sub-plugins

The shape an assistant-generated interface takes is a `ViewApp`: a declarative view with **no executable of its own**. Each action binds exactly one of:

| Binding | Does | Can change |
| --- | --- | --- |
| `run` | Starts a job of an already-registered command app, with arguments templated from the view's form | Nothing outside that job |
| `ask` | Sends a templated prompt plus chosen context (`form`, `lastJob`) to the assistant; the reply appears in the view | Nothing outside the view |
| `revise` | Asks the assistant for a new version of this view; the proposed manifest is registered through the normal path | A registry replace, re-confirmed if it escalates |
| `act` | An open-ended request; in this proposal answered as text only | Only what its (future) tools can do, through the same gates |

`display` bindings project the last job's JSON-lines output into generic `table` or `log` blocks, refreshed live while the job runs.

A ViewApp's capabilities are **computed**: the union of what its run targets were granted, plus `agent` if any action talks to the assistant. It cannot under-declare, and a run action is refused if its target's grant grows after the view was approved.

`owner: {plugin, conversation}` records which assistant conversation created an app. Removed and cleared ephemeral manifests are archived per owner, so a past conversation's tools can be restored later, going through the gate again.

### Assistant integration (follow-up)

`internal/assistant` defines a provider-neutral interface (`Name`, `DisplayName`, `Complete`) and ships only a deterministic `Scripted` provider for tests and the demo. The intended follow-up:

- An on-device **`AssistantApp`** (system tier) with a conversation view, an "Ask <provider>" action on every view (the current ViewDocument is the context), and provider adapters for Anthropic, OpenAI and any OpenAI-compatible endpoint (including local models). The display name follows the configured provider ("Claude wants to add a view").
- The assistant runs **as its own sandboxed process** with network access only to its provider and a scoped token for the local registration API, so "the agent is just another client" holds on the device itself. API keys stay in the state directory, never in views, logs or API responses.
- One tool surface (`list_plugins`, `get_manifest_schema`, `register_plugin`, `run_job`, `read_view`) exposed both to on-device providers and as an MCP server for external agents.
- Offline, `run` actions keep working; assistant actions show as disabled with a reason.

### Sandbox

`internal/sandbox` wraps every command of an `installed` or `ephemeral` app (and of `system` apps with `-sandbox-system`). The bubblewrap implementation:

- unshares every namespace, and shares the network only if `network` was granted;
- mounts an empty root with read-only `/usr`, `/bin`, `/lib*` and loader config, the command roots read-only, a private `/tmp`, `/proc`, and a minimal `/dev` plus granted device nodes;
- clears the environment, dies with the daemon, and starts a new session.

`-sandbox auto` (default) probes bubblewrap and falls back to a no-op runner that logs `running plugin command WITHOUT a sandbox` for every job. Views then say so too, instead of claiming sandboxing. `-sandbox bwrap` refuses to start without a working bubblewrap. Sandbox tests skip when bubblewrap is unavailable.

### Rate limiting

A token bucket per plugin id (`-job-rate` per minute, `-job-burst`, default 30/5) is enforced by the daemon on every job start, whoever asks. Assistant actions have their own bucket per view. Over the limit: `429 rate_limited` with `Retry-After`.

### Live refresh

Every registry change emits `registry.changed` on the existing SSE stream (`{version, pluginId, change, tier}`, with `change` one of `added`, `replaced`, `removed`, `pending`, `rejected`, `promoted`) and bumps the revision of sessions on the home view or on the affected app, which also emits `view.changed`. The Web UI and TUI refetch their snapshot on either event. Stale actions against an old view get the existing `409 stale_view`.

### Running jobs when a plugin changes

- **Replaced**: running jobs finish on the definition they started with (each job records `pluginVersion`); new jobs use the new version.
- **Removed**: running jobs are canceled with `canceled: plugin removed` (their capability grant is revoked with the plugin), sessions viewing the app return home, and the manifest is archived.

## How this fits "declared extension points, not arbitrary runtime mutation"

[PR #10](https://github.com/flipperdevices/flipctl/pull/10) (closed, not merged) argues that "the plugin contract should move toward declared extension points rather than arbitrary runtime mutation", that plugins should not own state authority, and that capabilities are explicit, bounded effect boundaries. This proposal keeps that shape:

- **The core stays authoritative.** A registration is a request; the daemon validates it, decides its effective capabilities, gates it, and owns every resulting job and projection. Plugins cannot touch sessions, the registry or other plugins.
- **The extension point is the same declared one.** A runtime manifest is the same `flipctl.app/v1alpha1` document a reviewer would read in `plugins/`, checked by the same parser and schema. There is no code loading, no hooks, and no UI injection.
- **Only the timing changes.** "Declared" means the manifest states what it is and what it needs before anything runs; it does not have to mean "known at boot". Registration-time declaration plus device confirmation gives the same guarantee as boot-time declaration plus image review, scoped to the ephemeral tier.

## How this addresses the unsandboxed-community-plugin gap

[PR #13](https://github.com/flipperdevices/flipctl/pull/13) calls `flipctld`'s privileges combined with unsandboxed community plugins "the most serious current gap… a hole, not a neutral gap", notes that its declared `[permissions]` are not yet enforced, and suggests D-Bus policy plus polkit with per-plugin identities for system integrations. Runtime registration would widen that hole if it did not close it first, so this proposal makes declared capabilities enforced:

- Non-system commands run under bubblewrap with only their granted network and devices.
- Sensitive capabilities need a physical confirmation, and escalation needs another.
- The tier (`system` / `installed` / `ephemeral`) changes execution, not just UI grouping.

It does not replace the D-Bus/polkit direction for system services; the two compose (a sandboxed process can still be a D-Bus client with its own identity). Running each plugin under its own UID (`DynamicUser=`) remains open.

## Security model

**Assets**: the device's radios, network, storage, the user's data, and the daemon's own privileges.

**Who can register**: by default only loopback clients that address the daemon by a local host name. `-allow-remote-registration` widens this to the network; cross-origin browser requests are refused either way, and registration routes never send CORS headers. This blocks drive-by registration and DNS rebinding from web pages, which matters because the rest of the API sends `Access-Control-Allow-Origin: *`.

**What a registration can do without a confirmation**: run a binary that already exists inside a command root, with no network, no extra devices, a read-only view of system directories, under rate limits. Anything more needs the device button.

**Threats and mitigations**:

| Threat | Mitigation |
| --- | --- |
| Malicious or buggy manifest | Schema + strict decode + semantic validation; no shell; executables confined to command roots |
| Privilege use by a plugin | bubblewrap with granted capabilities only; system tier immutable at runtime |
| Silent capability escalation | Replacements that add capabilities go pending; ViewApp grants are computed; escalated run targets are refused |
| Web page or remote host registering plugins | Loopback-only + Host check + Origin/Fetch-Metadata checks; no CORS |
| Runaway or abusive use | Per-plugin job rate limits; per-view assistant rate limits; bounded output buffers |
| **Prompt injection** (data from packets, SSIDs, banners or job output steering the assistant into registering or revising a plugin) | The defense is the gate, not the model: anything sensitive needs a physical press; agent turns start only from a human action; computed capabilities cannot be talked down |
| Persistence of runtime tools | Ephemeral apps are archived at startup; promotion needs a confirmation; grants never survive a restart |

**Known limits of this proposal**:

- The emulated confirmation is a session action. Any local client that can reach the session API can press it, so it is a stand-in for the UX, not a security boundary. The boundary exists only when confirmations come from a hardware input the HTTP API cannot synthesize.
- The no-op sandbox fallback enforces nothing; it is loud about it, but production profiles should use `-sandbox bwrap`.
- The daemon still runs all plugins under its own UID.
- bubblewrap needs unprivileged user namespaces; inside containers it needs relaxed confinement (see `examples/runtime-registration/compose.bwrap.yaml`). On a device the daemon runs on the host.

## Run the demo

```bash
./examples/runtime-registration/demo.sh                     # sandbox auto (falls back to no-op in a default container)
DEMO_SANDBOX=bwrap ./examples/runtime-registration/demo.sh  # nest bubblewrap inside the demo container
DEMO_PAUSE=3 ./examples/runtime-registration/demo.sh        # time to watch http://localhost:8080 or `make tui`
```

The script starts the compose stack with fake commands, then, without restarting anything: tries to register from the host (refused, `403`), registers `field.lora-rx` (pending: it declares `/dev/ttyLoRa0`), confirms it with the emulated device button, runs it, registers a generated calibration ViewApp on top of it (pending: device + assistant), captures packets, asks the scripted assistant which reading was best, replaces the receiver with v0.2.0 (applied immediately: same capabilities), and removes both (archived). Each step's ViewDocument is saved and rendered with `flipctl-tui --render-viewdoc`. A recorded run is in [`docs/evidence/runtime-registration.md`](evidence/runtime-registration.md).

## Open questions

1. **Off by default per OS profile?** Registration is on whenever a state directory exists. Should production images ship with it off, or with only the ephemeral tier disabled, and enable it per profile ("field kit", "lab")?
2. **Unix socket only?** A root-owned Unix socket with peer credentials (`SO_PEERCRED`) would identify the registering process and remove HTTP origin concerns entirely. Should registration move off TCP?
3. **Signing for promotion?** Should `installed` require a signed manifest (by the user's key, or a community key), with the device confirmation as a second factor?
4. **Does `ViewDocument` need a streaming block?** Live telemetry currently re-projects a bounded `table` on every output line. A block that streams rows (append-only, bounded) would cut refetches and suit the 256×144 display. Is that worth a vocabulary change?
5. **Grant scope.** Should a confirmation be rememberable per conversation (for example "allow `/dev/ttyLoRa0` for this conversation"), and how is that shown on the device?
6. **Per-plugin identity.** Run sandboxed commands under per-plugin UIDs so D-Bus/polkit (PR #13) can tell them apart?
7. **Assistant tools for `act`.** Which tools does an assistant get on the device, and which of them need a confirmation per call rather than per registration?
8. **Licensing.** The repository has no LICENSE yet, so the terms for contributions like this one are undefined.
