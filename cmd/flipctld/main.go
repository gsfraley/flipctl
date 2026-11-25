package main

import (
	"flag"
	"fmt"
	"github.com/flipperdevices/flipctl/internal/api"
	"github.com/flipperdevices/flipctl/internal/assistant"
	"github.com/flipperdevices/flipctl/internal/plugin"
	"github.com/flipperdevices/flipctl/internal/sandbox"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

var version = "dev"

func main() {
	addr := flag.String("addr", ":8080", "")
	plug := flag.String("plugins", "plugins", "")
	web := flag.String("web", "web/dist", "")
	stateDir := flag.String("state-dir", defaultStateDir(), "directory for runtime-registered (ephemeral and installed) plugins; empty disables the registration API")
	keepEphemeral := flag.Bool("keep-ephemeral", false, "keep ephemeral plugins across daemon restarts (they are re-checked and sensitive ones need confirmation again)")
	commandRoots := flag.String("command-roots", "fakecmds", "comma-separated directories that executables of runtime-registered command apps must live in")
	sandboxMode := flag.String("sandbox", "auto", "sandbox for runtime-registered plugin commands: auto, bwrap or none")
	sandboxSystem := flag.Bool("sandbox-system", false, "also sandbox commands of system plugins from -plugins")
	jobRate := flag.Float64("job-rate", 30, "job starts per minute allowed per plugin; 0 disables the limit")
	jobBurst := flag.Int("job-burst", 5, "burst of job starts allowed per plugin")
	allowRemote := flag.Bool("allow-remote-registration", false, "accept plugin registration from non-loopback clients")
	emulatedConfirm := flag.Bool("emulated-device-confirm", false, "offer the device confirmation as a session action standing in for a physical button (emulated builds only)")
	assistantProvider := flag.String("assistant", "", "assistant provider for ViewApp ask/revise/act actions: empty (none) or scripted")
	assistantLabel := flag.String("assistant-label", "", "display name for the assistant provider")
	flag.Parse()
	apps, err := plugin.LoadDir(*plug)
	if err != nil {
		log.Fatal(err)
	}
	sb, err := sandbox.Select(*sandboxMode)
	if err != nil {
		log.Fatal(err)
	}
	opts := api.Options{
		CommandRoots:            splitList(*commandRoots),
		Sandbox:                 sb,
		SandboxSystem:           *sandboxSystem,
		JobRate:                 *jobRate,
		JobBurst:                *jobBurst,
		AllowRemoteRegistration: *allowRemote,
		EmulatedDeviceConfirm:   *emulatedConfirm,
	}
	switch *assistantProvider {
	case "":
	case "scripted":
		opts.Assistant = assistant.Scripted{Label: *assistantLabel}
	default:
		log.Fatalf("unknown -assistant %q", *assistantProvider)
	}
	if _, e := os.Stat(*web); e == nil {
		opts.Static = http.FileServer(http.Dir(*web))
	}
	var store *plugin.Store
	if *stateDir != "" {
		if store, err = plugin.OpenStore(*stateDir); err != nil {
			log.Fatalf("state dir: %v", err)
		}
		opts.Store = store
		if !*keepEphemeral {
			ids, err := store.ClearEphemeral()
			if err != nil {
				slog.Warn("clearing ephemeral plugins", "error", err)
			}
			if len(ids) > 0 {
				slog.Info("ephemeral plugins from the previous run were archived, not activated", "ids", ids)
			}
		}
	}
	srv := api.NewWithOptions(plugin.NewRegistry(plugin.SystemEntries(apps, "catalog:"+*plug)), opts)
	if store != nil {
		installed, errs := store.Load(plugin.TierInstalled)
		for _, e := range errs {
			slog.Warn("installed plugin not loaded", "error", e)
		}
		srv.AdoptInstalled(installed)
		if *keepEphemeral {
			kept, errs := store.Load(plugin.TierEphemeral)
			for _, e := range errs {
				slog.Warn("kept ephemeral plugin not loaded", "error", e)
			}
			srv.RestoreEphemeral(kept)
		}
	}
	snap := srv.Registry().Snapshot()
	slog.Info("flipctld starting", "version", version, "apps", len(snap.Entries), "registration", store != nil, "stateDir", *stateDir, "sandbox", sb.Name(), "remoteRegistration", *allowRemote, "emulatedDeviceConfirm", *emulatedConfirm, "assistant", *assistantProvider)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}

// defaultStateDir follows systemd's StateDirectory= when set, and otherwise
// uses a per-user directory under the temporary directory.
func defaultStateDir() string {
	if d := os.Getenv("STATE_DIRECTORY"); d != "" {
		return strings.Split(d, ":")[0]
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("flipctl-%d", os.Getuid()))
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
