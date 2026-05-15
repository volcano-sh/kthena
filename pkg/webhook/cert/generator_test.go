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
	"math/big"
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
	assert.Positive(t, caCert.SerialNumber.Sign())

	// Verify server certificate
	certBlock, _ := pem.Decode(bundle.CertPEM)
	require.NotNil(t, certBlock)
	assert.Equal(t, "CERTIFICATE", certBlock.Type)

	serverCert, err := x509.ParseCertificate(certBlock.Bytes)
	require.NoError(t, err)
	assert.False(t, serverCert.IsCA)
	assert.Equal(t, dnsNames, serverCert.DNSNames)
	assert.Equal(t, dnsNames[0], serverCert.Subject.CommonName)
	assert.Positive(t, serverCert.SerialNumber.Sign())

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

	first, err := GenerateSelfSignedCertificate(dnsNames)
	require.NoError(t, err)
	second, err := GenerateSelfSignedCertificate(dnsNames)
	require.NoError(t, err)

	firstCABlock, _ := pem.Decode(first.CAPEM)
	firstCertBlock, _ := pem.Decode(first.CertPEM)
	secondCABlock, _ := pem.Decode(second.CAPEM)
	secondCertBlock, _ := pem.Decode(second.CertPEM)
	require.NotNil(t, firstCABlock)
	require.NotNil(t, firstCertBlock)
	require.NotNil(t, secondCABlock)
	require.NotNil(t, secondCertBlock)

	firstCA, err := x509.ParseCertificate(firstCABlock.Bytes)
	require.NoError(t, err)
	firstServer, err := x509.ParseCertificate(firstCertBlock.Bytes)
	require.NoError(t, err)
	secondCA, err := x509.ParseCertificate(secondCABlock.Bytes)
	require.NoError(t, err)
	secondServer, err := x509.ParseCertificate(secondCertBlock.Bytes)
	require.NoError(t, err)

	assert.Positive(t, firstCA.SerialNumber.Sign())
	assert.Positive(t, firstServer.SerialNumber.Sign())
	assert.NotEqual(t, big.NewInt(1), firstCA.SerialNumber)
	assert.NotEqual(t, big.NewInt(2), firstServer.SerialNumber)
	assert.NotEqual(t, firstCA.SerialNumber, secondCA.SerialNumber)
	assert.NotEqual(t, firstServer.SerialNumber, secondServer.SerialNumber)
}

func TestGenerateSelfSignedCertificate_EmptyDNSNames(t *testing.T) {
	dnsNames := []string{}

	bundle, err := GenerateSelfSignedCertificate(dnsNames)
	require.Error(t, err)
	require.Nil(t, bundle)
	assert.Contains(t, err.Error(), "dnsNames cannot be empty")
}
