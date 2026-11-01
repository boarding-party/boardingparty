// Package deploy embeds the director manifest so the CLI can install it.
// The file stays here so `kubectl apply -f deploy/director.yaml` still works.
package deploy

import _ "embed"

// Director is the director's manifest, unrendered (no namespace, :main tags).
//
//go:embed director.yaml
var Director []byte
