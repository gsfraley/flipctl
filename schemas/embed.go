// Package schemas exposes the checked-in JSON Schemas to Go code so the daemon
// validates against exactly the files that are published in this directory.
package schemas

import "embed"

//go:embed view-document.json plugin-manifest.json
var FS embed.FS

const (
	ViewDocumentID   = "https://flipctl.dev/schemas/view-document.json"
	PluginManifestID = "https://flipctl.dev/schemas/plugin-manifest.json"
)
