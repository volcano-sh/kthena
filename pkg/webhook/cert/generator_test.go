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

package cert

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateSelfSignedCertificate(t *testing.T) {
	dnsNames := []string{
		"webhook.default.svc",
		"webhook.default.svc.cluster.local",
	}

	bundle, err := GenerateSelfSignedCertificate(dnsNames)
	require.NoError(t, err)
	require.NotNil(t, bundle)

	// Verify CA certificate
	caBlock, _ := pem.Decode(bundle.CAPEM)
	require.NotNil(t, caBlock)
	assert.Equal(t, "CERTIFICATE", caBlock.Type)

	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	require.NoError(t, err)
	assert.True(t, caCert.IsCA)
	assert.Equal(t, "kthena-webhook-ca", caCert.Subject.CommonName)

	// Verify server certificate
	certBlock, _ := pem.Decode(bundle.CertPEM)
	require.NotNil(t, certBlock)
	assert.Equal(t, "CERTIFICATE", certBlock.Type)

	serverCert, err := x509.ParseCertificate(certBlock.Bytes)
	require.NoError(t, err)
	assert.False(t, serverCert.IsCA)
	assert.Equal(t, dnsNames, serverCert.DNSNames)
	assert.Equal(t, dnsNames[0], serverCert.Subject.CommonName)

	// Verify server key
	keyBlock, _ := pem.Decode(bundle.KeyPEM)
	require.NotNil(t, keyBlock)
	assert.Equal(t, "RSA PRIVATE KEY", keyBlock.Type)

	_, err = x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	require.NoError(t, err)

	// Verify that server certificate can be verified by CA
	roots := x509.NewCertPool()
	roots.AddCert(caCert)

	opts := x509.VerifyOptions{
		DNSName: dnsNames[0],
		Roots:   roots,
	}

	_, err = serverCert.Verify(opts)
	require.NoError(t, err)
}

func TestGenerateSelfSignedCertificate_SingleDNSName(t *testing.T) {
	dnsNames := []string{"webhook.default.svc"}

	bundle, err := GenerateSelfSignedCertificate(dnsNames)
	require.NoError(t, err)
	require.NotNil(t, bundle)

	certBlock, _ := pem.Decode(bundle.CertPEM)
	serverCert, err := x509.ParseCertificate(certBlock.Bytes)
	require.NoError(t, err)

	assert.Equal(t, dnsNames, serverCert.DNSNames)
}

func TestGenerateSelfSignedCertificate_MultipleDNSNames(t *testing.T) {
	dnsNames := []string{
		"webhook1.default.svc",
		"webhook2.default.svc",
		"webhook3.default.svc.cluster.local",
	}

	bundle, err := GenerateSelfSignedCertificate(dnsNames)
	require.NoError(t, err)
	require.NotNil(t, bundle)

	certBlock, _ := pem.Decode(bundle.CertPEM)
	serverCert, err := x509.ParseCertificate(certBlock.Bytes)
	require.NoError(t, err)

	assert.Equal(t, dnsNames, serverCert.DNSNames)
	assert.Equal(t, dnsNames[0], serverCert.Subject.CommonName)
}

func TestGenerateSelfSignedCertificate_RandomSerialNumbers(t *testing.T) {
	dnsNames := []string{"webhook.default.svc"}

	bundle1, err := GenerateSelfSignedCertificate(dnsNames)
	require.NoError(t, err)

	bundle2, err := GenerateSelfSignedCertificate(dnsNames)
	require.NoError(t, err)

	// Parse CA certs
	ca1Block, _ := pem.Decode(bundle1.CAPEM)
	ca1, err := x509.ParseCertificate(ca1Block.Bytes)
	require.NoError(t, err)

	ca2Block, _ := pem.Decode(bundle2.CAPEM)
	ca2, err := x509.ParseCertificate(ca2Block.Bytes)
	require.NoError(t, err)

	// Parse server certs
	srv1Block, _ := pem.Decode(bundle1.CertPEM)
	srv1, err := x509.ParseCertificate(srv1Block.Bytes)
	require.NoError(t, err)

	srv2Block, _ := pem.Decode(bundle2.CertPEM)
	srv2, err := x509.ParseCertificate(srv2Block.Bytes)
	require.NoError(t, err)

	// Serial numbers must be positive
	assert.True(t, ca1.SerialNumber.Sign() > 0, "CA serial must be positive")
	assert.True(t, srv1.SerialNumber.Sign() > 0, "server serial must be positive")

	// Serial numbers from different calls must differ
	assert.NotEqual(t, ca1.SerialNumber, ca2.SerialNumber, "CA serials should differ across calls")
	assert.NotEqual(t, srv1.SerialNumber, srv2.SerialNumber, "server serials should differ across calls")

	// CA and server serials within the same bundle must differ
	assert.NotEqual(t, ca1.SerialNumber, srv1.SerialNumber, "CA and server serials should differ")
}

func TestGenerateSelfSignedCertificate_EmptyDNSNames(t *testing.T) {
	dnsNames := []string{}

	bundle, err := GenerateSelfSignedCertificate(dnsNames)
	require.Error(t, err)
	require.Nil(t, bundle)
	assert.Contains(t, err.Error(), "dnsNames cannot be empty")
}
