/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package sessionsticky

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

// MappingKey returns an opaque store key for a ModelRoute and raw session material.
func MappingKey(route types.NamespacedName, sessionMaterial string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%s|%s", route.Namespace, route.Name, sessionMaterial)))
	return "kthena/sticky/" + hex.EncodeToString(sum[:])
}
