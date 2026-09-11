package v1alpha1

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Palette-deployed NodePrepProfiles carry integers that became strings on
// the way through pack templating (spec.eastWest.numVFs: "0" is rejected by
// the CRD's type: integer). Rather than loosening every Go consumer to an
// IntOrString, the profile spec structs normalize numeric strings back into
// JSON numbers at decode time — every profile read (controller adoption and
// the agent's fetchProfile) goes through encoding/json, so these
// UnmarshalJSON methods are the single choke point. The CRD schema accepts
// both shapes (anyOf integer|string + x-kubernetes-int-or-string); Go always
// sees plain ints and marshals ints.

// flexNormalizeIntFields rewrites a JSON object so the named fields carrying
// numeric strings become JSON numbers. Non-object input, absent fields,
// numbers, and null pass through untouched.
func flexNormalizeIntFields(b []byte, fields map[string]bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	changed := false
	for name := range fields {
		raw, ok := m[name]
		if !ok {
			continue
		}
		var s string
		if len(raw) == 0 || raw[0] != '"' {
			continue // number, bool, null, object: leave for the typed decode
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			continue // not decodable as a string: leave for the typed decode
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("spec field %q: %q is not a valid integer", name, s)
		}
		if m[name], err = json.Marshal(n); err != nil {
			return nil, err
		}
		changed = true
	}
	if !changed {
		return b, nil
	}
	return json.Marshal(m)
}

// flexNormalizeBoolFields is flexNormalizeIntFields' boolean twin: the same
// pack-templating mechanism that stringifies integers also renders the
// offloadEngine boolean as a string ("true"/"false"), and legacy profiles
// still carry the pre-0.1.84 enum ("none"/"sf"/"smf"). Legacy values map by
// their old meaning — sf/smf expressed an active offload engine (→ true),
// none did not (→ false); anything else is an error naming the field.
func flexNormalizeBoolFields(b []byte, fields map[string]bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	changed := false
	for name := range fields {
		raw, ok := m[name]
		if !ok {
			continue
		}
		var s string
		if len(raw) == 0 || raw[0] != '"' {
			continue // bool, number, null, object: leave for the typed decode
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		var v bool
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "sf", "smf":
			v = true
		case "false", "0", "none", "":
			v = false
		default:
			return nil, fmt.Errorf("spec field %q: %q is not a boolean", name, s)
		}
		bv, merr := json.Marshal(v)
		if merr != nil {
			return nil, merr
		}
		m[name] = bv
		changed = true
	}
	if !changed {
		return b, nil
	}
	return json.Marshal(m)
}

// flexIntFields lists the numeric fields each spec struct normalizes. Keep in
// step with the CRD's int-or-string fields (manifests/crd-nodeprepprofile.yaml).
var (
	flexEastWestFields       = map[string]bool{"numVFs": true, "mtu": true, "planesNum": true, "nicBreakout": true}
	flexNorthSouthFields     = map[string]bool{"numVFs": true}
	flexNorthSouthBoolFields = map[string]bool{"offloadEngine": true}
	flexHugepagesFields      = map[string]bool{"pages1G": true, "pages2M": true}
	flexPolicyFields         = map[string]bool{"maxConcurrentFlashes": true}
	flexControlPlaneFields   = map[string]bool{"expectedCount": true}
)

// The alias indirection is the standard custom-unmarshal escape hatch: the
// alias type has no UnmarshalJSON, so the inner decode cannot recurse.

func (e *EastWestSpec) UnmarshalJSON(b []byte) error {
	type alias EastWestSpec
	raw, err := flexNormalizeIntFields(b, flexEastWestFields)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, (*alias)(e))
}

func (n *NorthSouthSpec) UnmarshalJSON(b []byte) error {
	type alias NorthSouthSpec
	raw, err := flexNormalizeIntFields(b, flexNorthSouthFields)
	if err != nil {
		return err
	}
	if raw, err = flexNormalizeBoolFields(raw, flexNorthSouthBoolFields); err != nil {
		return err
	}
	return json.Unmarshal(raw, (*alias)(n))
}

func (h *HugepagesSpec) UnmarshalJSON(b []byte) error {
	type alias HugepagesSpec
	raw, err := flexNormalizeIntFields(b, flexHugepagesFields)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, (*alias)(h))
}

func (p *PolicySpec) UnmarshalJSON(b []byte) error {
	type alias PolicySpec
	raw, err := flexNormalizeIntFields(b, flexPolicyFields)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, (*alias)(p))
}

func (c *ControlPlaneSpec) UnmarshalJSON(b []byte) error {
	type alias ControlPlaneSpec
	raw, err := flexNormalizeIntFields(b, flexControlPlaneFields)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, (*alias)(c))
}
