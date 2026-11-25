# Runtime registration evidence

Recorded run of [`examples/runtime-registration/demo.sh`](../../examples/runtime-registration/demo.sh) against the compose stack with fake commands, plus a live TUI and a live browser watching the same daemon. No physical hardware is involved.

## Environment

- Fedora (x86_64), rootless podman with `podman-compose`, Go 1.26 for local builds (the branch also builds and tests with Go 1.22.12, the version pinned in `go.mod` and CI).
- Stack: `docker-compose.yml` + `examples/runtime-registration/compose.yaml` + `compose.bwrap.yaml` (`DEMO_SANDBOX=bwrap`), so plugin commands run under bubblewrap inside the container.
- Command: `DEMO_SANDBOX=bwrap DEMO_PAUSE=3 ./examples/runtime-registration/demo.sh`

## What the run shows

| Step | Result | Evidence |
| --- | --- | --- |
| Registration from the host (not loopback inside the container) | `403 registration_local_only` | [transcript](runtime-registration/demo-transcript.txt) |
| `PUT field.lora-rx` v0.1.0 declaring `/dev/ttyLoRa0` | `202`, listed under "Waiting for device confirmation", not active | [02-pending-lora-rx.txt](runtime-registration/02-pending-lora-rx.txt) |
| Emulated device button (`device.confirm_registration`) | Active, listed under "Runtime-registered (ephemeral)" | [03-home-with-lora-rx.txt](runtime-registration/03-home-with-lora-rx.txt) |
| Run it | Job `j-1` done; `result.sandbox = bwrap` | [05-lora-rx-done.txt](runtime-registration/05-lora-rx-done.txt) |
| `PUT` generated calibration ViewApp | `202`; computed capabilities `devices: [/dev/ttyLoRa0]`, `agent: true` | [06-pending-calibration.txt](runtime-registration/06-pending-calibration.txt) |
| Capture from the view | Table fills while the job streams | [08-calibration-capturing.txt](runtime-registration/08-calibration-capturing.txt) |
| Ask the (scripted) assistant | Reply projected as a notice in the view | [09-calibration-answer.txt](runtime-registration/09-calibration-answer.txt) |
| Replace `field.lora-rx` with v0.2.0 (same capabilities) | `200`, applied immediately, new `sf` field | [10-lora-rx-v2-form.txt](runtime-registration/10-lora-rx-v2-form.txt) |
| `DELETE` both | Home back to the catalog; both manifests archived and restorable | [11-home-after.txt](runtime-registration/11-home-after.txt) |

The daemon was never restarted; the registry version went from 1 to 6.

## Rendering method

Each `NN-*.txt` file is the observer session's ViewDocument at that step, fetched from `/api/v1/sessions/{id}/view` and rendered with `flipctl-tui --render-viewdoc --render-color plain` inside the container, the same renderer used for `docs/evidence/tui.txt`.

## Live frontends

Two more clients held their own sessions on the home view for the whole run and were never restarted or reloaded:

- **Interactive TUI** (`flipctl-tui -addr http://localhost:8080` in a 140×48 tmux pane, captured with `tmux capture-pane` about one second after each registry change): [start](runtime-registration/tui-live-00-start.txt), [receiver active](runtime-registration/tui-live-01-registry-v2.txt), [calibration view active](runtime-registration/tui-live-02-registry-v3.txt), [receiver replaced](runtime-registration/tui-live-03-registry-v4.txt), [both removed](runtime-registration/tui-live-04-registry-v6.txt).
- **Web UI** (headless Chromium driven over the DevTools protocol, one page load, screenshots after registry changes): [start](runtime-registration/web-live-00-start.png), [both apps registered](runtime-registration/web-live-02-registry-v3.png), [both removed](runtime-registration/web-live-04-registry-v6.png). [`web-live-page-origin.txt`](runtime-registration/web-live-page-origin.txt) records the same `performance.timeOrigin` at the start and end of the run, so the page was not reloaded.

The capture harness (tmux and DevTools scripts) is not part of this change; the TUI and Web behavior it shows is covered by `TestWatchEventsRefreshesOnRegistryChanged`, `TestViewFillsExactlyTheWindowHeight` and the Web entrypoint test.

## Fallback without bubblewrap

Running the same demo without `compose.bwrap.yaml` (bubblewrap cannot mount `/proc` in a default rootless container), the daemon starts with `sandbox=none`, every runtime job logs `running plugin command WITHOUT a sandbox: declared capabilities are not enforced`, jobs report `result.sandbox = none`, and the ephemeral notice on every frontend reads "run WITHOUT a sandbox on this build" instead of claiming sandboxing.

## Not proven here

- No hardware: the device confirmation is the emulated session action, and `/dev/ttyLoRa0` does not exist in the container (bubblewrap's `--dev-bind-try` skips it).
- The assistant is the deterministic scripted provider; no model was called.
