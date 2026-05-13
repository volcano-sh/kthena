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

// MappingKey returns an opaque store key for a ModelServer and raw session material.
func MappingKey(modelServer types.NamespacedName, sessionMaterial string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%s|%s", modelServer.Namespace, modelServer.Name, sessionMaterial)))
	return "kthena/sticky/" + hex.EncodeToString(sum[:])
}
