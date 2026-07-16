/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/x509"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateSelfSignedCertificate(t *testing.T) {
	certificate, err := generateSelfSignedCertificate()
	require.NoError(t, err)
	require.Len(t, certificate.Certificate, 1)
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	require.NoError(t, err)
	assert.Equal(t, "kthena-external-provider-mock", leaf.Subject.CommonName)
	assert.Contains(t, leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	assert.True(t, leaf.NotBefore.Before(time.Now()))
	assert.True(t, leaf.NotAfter.After(time.Now().Add(12*time.Hour)))
}
