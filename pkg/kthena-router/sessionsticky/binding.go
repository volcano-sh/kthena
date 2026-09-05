/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package sessionsticky

import "fmt"

// Redis hash field names for session sticky bindings (same style as rate-limit token bucket).
const (
	redisFieldModelServer = "modelServer"
	redisFieldPod         = "pod"
)

// Binding pins a session to a ModelServer (same-namespace name) and Pod.
type Binding struct {
	ModelServer string
	Pod         string
}

// Valid reports whether both fields are set.
func (b Binding) Valid() bool {
	return b.ModelServer != "" && b.Pod != ""
}

// Equal reports whether two bindings refer to the same server and pod.
func (b Binding) Equal(other Binding) bool {
	return b.ModelServer == other.ModelServer && b.Pod == other.Pod
}

// String returns a debug representation.
func (b Binding) String() string {
	if !b.Valid() {
		return ""
	}
	return fmt.Sprintf("%s/%s", b.ModelServer, b.Pod)
}
