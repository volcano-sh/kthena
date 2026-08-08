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
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/cmd/kthena-router/app"
	"github.com/volcano-sh/kthena/pkg/kthena-router/webhook"
	webhookcert "github.com/volcano-sh/kthena/pkg/webhook/cert"
)

const validatingWebhookConfigurationName = "kthena-router-validating-webhook"

func main() {
	var (
		routerPort                         string
		tlsCert                            string
		tlsKey                             string
		enableWebhook                      bool
		enableGatewayAPI                   bool
		enableGatewayAPIInferenceExtension bool
		webhookPort                        int
		webhookCert                        string
		webhookKey                         string
		certSecretName                     string
		serviceName                        string
		debugPort                          int
		kubeAPIQPS                         float32
		kubeAPIBurst                       int
	)

	klog.InitFlags(nil)
	// Opt into fixed stderrthreshold behavior (kubernetes/klog#212).
	if err := flag.CommandLine.Set("legacy_stderr_threshold_behavior", "false"); err != nil {
		klog.Fatalf("Failed to set legacy_stderr_threshold_behavior: %v", err)
	}
	if err := flag.CommandLine.Set("stderrthreshold", "INFO"); err != nil {
		klog.Fatalf("Failed to set stderrthreshold: %v", err)
	}
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.StringVar(&routerPort, "port", "8080", "Server listen port")
	pflag.StringVar(&tlsCert, "tls-cert", "", "TLS certificate file path")
	pflag.StringVar(&tlsKey, "tls-key", "", "TLS key file path")
	pflag.BoolVar(&enableWebhook, "enable-webhook", true, "Enable built-in admission webhook server")
	pflag.BoolVar(&enableGatewayAPI, "enable-gateway-api", false, "Enable Gateway API related features")
	pflag.BoolVar(&enableGatewayAPIInferenceExtension, "enable-gateway-api-inference-extension", false, "Enable Gateway API Inference Extension features (requires --enable-gateway-api)")
	pflag.IntVar(&webhookPort, "webhook-port", 8443, "The port for the webhook server")
	pflag.StringVar(&webhookCert, "webhook-tls-cert-file", "/etc/tls/tls.crt", "Path to the webhook TLS certificate file")
	pflag.StringVar(&webhookKey, "webhook-tls-private-key-file", "/etc/tls/tls.key", "Path to the webhook TLS private key file")
	pflag.StringVar(&certSecretName, "cert-secret-name", "kthena-router-webhook-certs", "Name of the secret to store auto-generated webhook certificates")
	pflag.StringVar(&serviceName, "webhook-service-name", "kthena-router-webhook", "Service name for the webhook server")
	pflag.IntVar(&debugPort, "debug-port", 15000, "The port for the debug server (localhost only)")
	pflag.Float32Var(&kubeAPIQPS, "kube-api-qps", 0, "QPS to use while talking with kubernetes apiserver. If 0, use default value.")
	pflag.IntVar(&kubeAPIBurst, "kube-api-burst", 0, "Burst to use while talking with kubernetes apiserver. If 0, use default value.")
	defer klog.Flush()
	pflag.Parse()

	if (tlsCert != "" && tlsKey == "") || (tlsCert == "" && tlsKey != "") {
		klog.Fatal("tls-cert and tls-key must be specified together")
	}

	if enableGatewayAPIInferenceExtension && !enableGatewayAPI {
		klog.Fatal("--enable-gateway-api-inference-extension requires --enable-gateway-api to be enabled")
	}

	if webhookPort <= 0 || webhookPort > 65535 {
		klog.Fatalf("invalid webhook port: %d", webhookPort)
	}

	if debugPort <= 0 || debugPort > 65535 {
		klog.Fatalf("invalid debug port: %d", debugPort)
	}

	pflag.CommandLine.VisitAll(func(f *pflag.Flag) {
		klog.Infof("Flag: %s, Value: %s", f.Name, f.Value.String())
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signalCh
		klog.Info("Received termination, signaling shutdown")
		cancel()
	}()

	if enableWebhook {
		go runWebhook(ctx, webhookPort, webhookCert, webhookKey, certSecretName, serviceName, kubeAPIQPS, kubeAPIBurst)
	} else {
		klog.Info("Webhook server is disabled")
	}

	app.NewServer(routerPort, tlsCert != "" && tlsKey != "", tlsCert, tlsKey, enableGatewayAPI, enableGatewayAPIInferenceExtension, debugPort, kubeAPIQPS, kubeAPIBurst).Run(ctx)
}

// ensureWebhookCertificate generates a certificate secret if needed and returns the bundle.
func ensureWebhookCertificate(ctx context.Context, kubeClient kubernetes.Interface, secretName, serviceName string) (*webhookcert.CertBundle, error) {
	namespace := getNamespace()
	dnsNames := []string{
		fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace),
	}
	klog.Infof("Auto-generating certificate for webhook server (secret=%s service=%s)", secretName, serviceName)
	return webhookcert.EnsureCertificate(ctx, kubeClient, namespace, secretName, dnsNames)
}

// resolveServingCert returns the TLS material the webhook server should serve with,
// following the precedence: existing secret -> mounted cert files -> generate.
//
// Every branch returns the key pair in memory so the server never has to wait for a
// freshly created secret to be projected into the pod. See resolveServingCert in
// cmd/kthena-controller-manager for the full reasoning.
func resolveServingCert(ctx context.Context, kubeClient kubernetes.Interface, secretName, serviceName, certFile, keyFile string) (*webhookcert.CertBundle, error) {
	namespace := getNamespace()

	// 1. An existing secret is authoritative: it is the copy every replica agrees on.
	bundle, err := webhookcert.LoadCertBundleFromSecret(ctx, kubeClient, namespace, secretName)
	if err != nil {
		klog.Warningf("Error reading cert bundle from secret %s: %v", secretName, err)
	} else if bundle != nil && len(bundle.CertPEM) > 0 && len(bundle.KeyPEM) > 0 {
		klog.Infof("Loaded serving certificate from secret %s", secretName)
		return bundle, nil
	}

	// 2. Fall back to cert files supplied by an external cert manager. CAPEM is left
	// empty on purpose: that CA is published by whoever manages the files, so we must
	// not overwrite the webhook configuration.
	if fileExists(certFile) && fileExists(keyFile) {
		klog.Infof("Loading serving certificate from %s and %s", certFile, keyFile)
		return loadCertBundleFromFiles(certFile, keyFile)
	}

	// 3. Nothing exists yet: generate and persist so other replicas reuse this pair.
	return ensureWebhookCertificate(ctx, kubeClient, secretName, serviceName)
}

// loadCertBundleFromFiles reads an externally managed key pair off disk.
func loadCertBundleFromFiles(certFile, keyFile string) (*webhookcert.CertBundle, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read cert file %s: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file %s: %w", keyFile, err)
	}
	return &webhookcert.CertBundle{CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// runWebhook starts the webhook server and manages certificate acquisition with precedence:
// Secret -> existing cert files -> auto-generate new certs.
func runWebhook(ctx context.Context, port int, certFile, keyFile, secretName, serviceName string, kubeAPIQPS float32, kubeAPIBurst int) {
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to get kube config: %v", err)
	}
	// Set QPS and Burst if provided
	if kubeAPIQPS > 0 {
		config.QPS = kubeAPIQPS
	}
	if kubeAPIBurst > 0 {
		config.Burst = kubeAPIBurst
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to get kube client: %v", err)
	}

	bundle, err := resolveServingCert(ctx, kubeClient, secretName, serviceName, certFile, keyFile)
	if err != nil {
		klog.Fatalf("Failed to obtain webhook serving certificate: %v", err)
	}

	servingCert, err := tls.X509KeyPair(bundle.CertPEM, bundle.KeyPEM)
	if err != nil {
		klog.Fatalf("Failed to build webhook TLS key pair: %v", err)
	}

	if len(bundle.CAPEM) > 0 {
		// attempt to update the ValidatingWebhookConfiguration CA bundle.
		if err := webhookcert.UpdateValidatingWebhookCABundle(ctx, kubeClient, validatingWebhookConfigurationName, bundle.CAPEM); err != nil {
			klog.Warningf("Failed to update ValidatingWebhookConfiguration CA bundle: %v", err)
		} else {
			klog.Infof("Updated ValidatingWebhookConfiguration %s CA bundle", validatingWebhookConfigurationName)
		}
	}

	validator := webhook.NewKthenaRouterValidator(kubeClient, port)
	go validator.Run(ctx, servingCert)
	klog.Infof("Webhook server running on port %d", port)
}

// getNamespace returns the current pod namespace or "default".
func getNamespace() string {
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		return "default"
	}
	return ns
}

// fileExists returns true if the file exists.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
