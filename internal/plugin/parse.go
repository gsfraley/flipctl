package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"sync"

	"github.com/flipperdevices/flipctl/schemas"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// MaxManifestBytes bounds manifests accepted from any source.
const MaxManifestBytes = 64 << 10

var (
	manifestSchemaOnce sync.Once
	manifestSchema     *jsonschema.Schema
	manifestSchemaErr  error
)

// ManifestSchema returns the compiled schemas/plugin-manifest.json.
func ManifestSchema() (*jsonschema.Schema, error) {
	manifestSchemaOnce.Do(func() {
		b, err := fs.ReadFile(schemas.FS, "plugin-manifest.json")
		if err != nil {
			manifestSchemaErr = err
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			manifestSchemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(schemas.PluginManifestID, doc); err != nil {
			manifestSchemaErr = err
			return
		}
		manifestSchema, manifestSchemaErr = c.Compile(schemas.PluginManifestID)
	})
	return manifestSchema, manifestSchemaErr
}

// Parse is the single entry point for turning manifest bytes (YAML or JSON)
// into a validated App. Plugin directories and the registration API both use
// it, so an uploaded manifest is held to exactly the same rules as one on disk.
func Parse(b []byte) (App, error) {
	if len(b) > MaxManifestBytes {
		return App{}, fmt.Errorf("manifest exceeds %d bytes", MaxManifestBytes)
	}
	var raw any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return App{}, fmt.Errorf("parse manifest: %w", err)
	}
	if err := validateSchema(raw); err != nil {
		return App{}, err
	}
	var a App
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&a); err != nil {
		return App{}, fmt.Errorf("decode manifest: %w", err)
	}
	normalize(&a)
	if err := Validate(a); err != nil {
		return App{}, err
	}
	if a.Availability == nil {
		a.Availability = map[string]string{"state": "available"}
	}
	return a, nil
}

func validateSchema(raw any) error {
	s, err := ManifestSchema()
	if err != nil {
		return fmt.Errorf("manifest schema: %w", err)
	}
	// Round-trip through JSON so YAML scalars become the JSON types the
	// validator expects.
	j, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("manifest is not JSON-compatible: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		return err
	}
	if err := s.Validate(inst); err != nil {
		return &SchemaError{Err: err}
	}
	return nil
}

// SchemaError reports a manifest that does not match schemas/plugin-manifest.json.
type SchemaError struct{ Err error }

func (e *SchemaError) Error() string {
	return "manifest does not match plugin-manifest schema: " + e.Err.Error()
}
func (e *SchemaError) Unwrap() error { return e.Err }
