# ADR-0005: Runtime registration of declared, sandboxed plugins

Status: Proposed

`flipctld` accepts declarative plugin manifests at runtime through
`/api/v1/plugins`, in addition to the `-plugins` catalog loaded at startup.
Runtime-registered apps appear on every frontend through a `registry.changed`
SSE event without a daemon restart.

The extension point does not change: runtime manifests use the same
`flipctl.app/v1alpha1` format, the same `plugin.Parse` path and the same
`schemas/plugin-manifest.json` (JSON Schema 2020-12) as on-disk catalogs.
Apps still never ship UI code; they are projected into existing ViewDocument
blocks.

The registry is versioned and swapped atomically. Every app has a trust tier:
`system` (catalog, immutable at runtime), `installed` (promoted, persistent)
or `ephemeral` (registered at runtime, archived at daemon start). Ephemeral
apps are visibly marked on every frontend.

Manifests declare `capabilities` (`network`, `devices`, `rf_transmit`); the
daemon computes `agent` for apps that send data to an assistant, and computes
a `ViewApp`'s capabilities from what it composes. Sensitive or escalated
capabilities, and every promotion, wait for a local device confirmation that
no registration API call can provide. Commands of non-system apps run under a
sandbox (bubblewrap) with only their granted capabilities, and job starts are
rate-limited per plugin by the daemon.

Registration writes are accepted only from loopback clients using a local host
name unless explicitly enabled, and never from another browser origin.

Replacing an app leaves its running jobs on the definition they started with;
removing an app cancels its running jobs and archives its manifest.

See [runtime-registration.md](../runtime-registration.md) for the design,
security model and open questions.
