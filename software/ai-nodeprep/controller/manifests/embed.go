// Package manifests exposes the externally-enforced manifests to the
// controller binary.
//
// The CRDs are embedded so the chart's pre-upgrade hook job can apply them
// from the ordinary controller image: no kubectl image to mirror, no
// ConfigMap to mount, and the schema the job applies is always the one the
// binary was built with. manifests/ remains the externally-enforced source
// of truth — the embed adds a consumer, it does not move the source.
package manifests

import _ "embed"

//go:embed crd-nodeprepprofile.yaml
var nodePrepProfileCRD []byte

//go:embed crd-nodeprep.yaml
var nodePrepCRD []byte

// CRDs returns every CustomResourceDefinition the chart manages, in apply
// order. Each entry is the same document shipped under manifests/.
func CRDs() [][]byte {
	return [][]byte{nodePrepProfileCRD, nodePrepCRD}
}
