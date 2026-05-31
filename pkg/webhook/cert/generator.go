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
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"k8s.io/klog/v2"
)

const (
	// RSAKeySize is the size of the RSA key for certificate generation
	RSAKeySize = 2048
	// CertValidityYears is the number of years the certificate is valid
	CertValidityYears = 10
)

// CertBundle contains the certificate, key, and CA certificate
type CertBundle struct {
	// CertPEM is the PEM-encoded certificate
	CertPEM []byte
	// KeyPEM is the PEM-encoded private key
	KeyPEM []byte
	// CAPEM is the PEM-encoded CA certificate
	CAPEM []byte
}

// GenerateSelfSignedCertificate generates a self-signed certificate for webhook server
func GenerateSelfSignedCertificate(dnsNames []string) (*CertBundle, error) {
	if len(dnsNames) == 0 {
		return nil, fmt.Errorf("dnsNames cannot be empty")
	}

	klog.Infof("Generating self-signed certificate for DNS names: %v", dnsNames)

	// Generate CA certificate and key
	caKey, err := rsa.GenerateKey(rand.Reader, RSAKeySize)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA key: %w", err)
	}

	caSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA serial number: %w", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber: caSerial,
		Subject: pkix.Name{
			CommonName:   "kthena-webhook-ca",
			Organization: []string{"Volcano"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(CertValidityYears, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	// Generate server certificate and key
	serverKey, err := rsa.GenerateKey(rand.Reader, RSAKeySize)
	if err != nil {
		return nil, fmt.Errorf("failed to generate server key: %w", err)
	}

	serverSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate server serial number: %w", err)
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: serverSerial,
		Subject: pkix.Name{
			CommonName:   dnsNames[0],
			Organization: []string{"Volcano"},
		},
		DNSNames:    dnsNames,
		NotBefore:   time.Now(),
		NotAfter:    time.Now().AddDate(CertValidityYears, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create server certificate: %w", err)
	}

	// Encode CA certificate to PEM
	caCertPEM := new(bytes.Buffer)
	if err := pem.Encode(caCertPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caCertDER,
	}); err != nil {
		return nil, fmt.Errorf("failed to encode CA certificate: %w", err)
	}

	// Encode server certificate to PEM
	serverCertPEM := new(bytes.Buffer)
	if err := pem.Encode(serverCertPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: serverCertDER,
	}); err != nil {
		return nil, fmt.Errorf("failed to encode server certificate: %w", err)
	}

	// Encode server key to PEM
	serverKeyPEM := new(bytes.Buffer)
	if err := pem.Encode(serverKeyPEM, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(serverKey),
	}); err != nil {
		return nil, fmt.Errorf("failed to encode server key: %w", err)
	}

	klog.Info("Successfully generated self-signed certificate")

	return &CertBundle{
		CertPEM: serverCertPEM.Bytes(),
		KeyPEM:  serverKeyPEM.Bytes(),
		CAPEM:   caCertPEM.Bytes(),
	}, nil
}
