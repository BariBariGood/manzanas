// Package runspec parses the declarative YAML run-spec consumed by
// `manzanas run spec.yaml` and the MCP `run` tool into the wire types
// POST /v0/runs accepts (proto.RunSpec). Validation here is client-side
// convenience only — the daemon revalidates everything.
package runspec

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/BariBariGood/manzanas/proto"
)

// Parse decodes a YAML run-spec. Unknown fields are rejected so a typo
// ("step:" for "steps:") fails loudly instead of silently running an
// empty spec.
func Parse(data []byte) (proto.RunSpec, error) {
	var spec proto.RunSpec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return proto.RunSpec{}, fmt.Errorf("parse run-spec: %w", err)
	}
	if err := Validate(spec); err != nil {
		return proto.RunSpec{}, err
	}
	return spec, nil
}

// Validate applies the client-side checks that need no daemon state.
func Validate(spec proto.RunSpec) error {
	t := spec.Target
	if len(t.Labels) == 0 && t.UDID == "" && t.Runtime == "" && t.DeviceType == "" && t.Image == "" {
		return fmt.Errorf("run-spec: target requires at least one of labels, udid, runtime, or device_type")
	}
	for i, st := range spec.Steps {
		if st.Action == "" && st.MaestroFlow == "" {
			return fmt.Errorf("run-spec: steps[%d]: action is required", i)
		}
		if st.Action != "" && st.MaestroFlow != "" {
			return fmt.Errorf("run-spec: steps[%d]: action and maestro_flow are mutually exclusive", i)
		}
	}
	if app := spec.App; app != nil && app.Path == "" && app.BundleID == "" {
		return fmt.Errorf("run-spec: app requires path and/or bundle_id")
	}
	return nil
}
