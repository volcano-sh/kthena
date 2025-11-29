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
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

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
		routerPort       string
		tlsCert          string
		tlsKey           string
		enableWebhook    bool
		enableGatewayAPI bool
		webhookPort      int
		webhookCert      string
		webhookKey       string
		certSecretName   string
		serviceName      string
	)

	klog.InitFlags(nil)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.StringVar(&routerPort, "port", "8080", "Server listen port")
	pflag.StringVar(&tlsCert, "tls-cert", "", "TLS certificate file path")
	pflag.StringVar(&tlsKey, "tls-key", "", "TLS key file path")
	pflag.BoolVar(&enableWebhook, "enable-webhook", true, "Enable built-in admission webhook server")
	pflag.BoolVar(&enableGatewayAPI, "enable-gateway-api", false, "Enable Gateway API related features")
	pflag.IntVar(&webhookPort, "webhook-port", 8443, "The port for the webhook server")
	pflag.StringVar(&webhookCert, "webhook-tls-cert-file", "/etc/tls/tls.crt", "Path to the webhook TLS certificate file")
	pflag.StringVar(&webhookKey, "webhook-tls-private-key-file", "/etc/tls/tls.key", "Path to the webhook TLS private key file")
	pflag.StringVar(&certSecretName, "cert-secret-name", "kthena-router-webhook-certs", "Name of the secret to store auto-generated webhook certificates")
	pflag.StringVar(&serviceName, "webhook-service-name", "kthena-router-webhook", "Service name for the webhook server")
	defer klog.Flush()
	pflag.Parse()

	if (tlsCert != "" && tlsKey == "") || (tlsCert == "" && tlsKey != "") {
		klog.Fatal("tls-cert and tls-key must be specified together")
	}

	if webhookPort <= 0 || webhookPort > 65535 {
		klog.Fatalf("invalid webhook port: %d", webhookPort)
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
		go runWebhook(ctx, webhookPort, webhookCert, webhookKey, certSecretName, serviceName)
	} else {
		klog.Info("Webhook server is disabled")
	}

	app.NewServer(routerPort, tlsCert != "" && tlsKey != "", tlsCert, tlsKey, enableGatewayAPI).Run(ctx)
}

// ensureWebhookCertificate generates a certificate secret if needed and returns the CA bundle.
func ensureWebhookCertificate(ctx context.Context, kubeClient kubernetes.Interface, secretName, serviceName string) ([]byte, error) {
	namespace := getNamespace()
	dnsNames := []string{
		fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace),
	}
	klog.Infof("Auto-generating certificate for webhook server (secret=%s service=%s)", secretName, serviceName)
	return webhookcert.EnsureCertificate(ctx, kubeClient, namespace, secretName, dnsNames)
}

// runWebhook starts the webhook server and manages certificate acquisition with precedence:
// Secret -> existing cert files -> auto-generate new certs.
func runWebhook(ctx context.Context, port int, certFile, keyFile, secretName, serviceName string) {
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to get kube config: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to get kube client: %v", err)
	}

	namespace := getNamespace()
	var caBundle []byte

	// 1. Try secret first.
	if bundle, err := webhookcert.LoadCertBundleFromSecret(ctx, kubeClient, namespace, secretName); err != nil {
		klog.Warningf("Error reading CA bundle from secret %s: %v", secretName, err)
	} else if bundle != nil {
		klog.Infof("Loaded CA bundle from secret %s", secretName)
		caBundle = bundle.CAPEM
	}

	// 2. If not from secret, try existing cert file.
	if caBundle == nil {
		if !fileExists(certFile) || !fileExists(keyFile) {
			b, err := ensureWebhookCertificate(ctx, kubeClient, secretName, serviceName)
			if err != nil {
				klog.Fatalf("Failed to auto-generate webhook certificates: %v", err)
			}
			caBundle = b
		}
	}

	if caBundle != nil {
		// attempt to update the ValidatingWebhookConfiguration CA bundle.
		if err := webhookcert.UpdateValidatingWebhookCABundle(ctx, kubeClient, validatingWebhookConfigurationName, caBundle); err != nil {
			klog.Warningf("Failed to update ValidatingWebhookConfiguration CA bundle: %v", err)
		} else {
			klog.Infof("Updated ValidatingWebhookConfiguration %s CA bundle", validatingWebhookConfigurationName)
		}
	}

	validator := webhook.NewKthenaRouterValidator(kubeClient, port)

	// Wait for both cert and key files to exist (in case they are mounted by Kubernetes)
	ok := waitForCertsReady(keyFile, certFile)
	if !ok {
		klog.Fatalf("TLS cert/key files not found, webhook server cannot start")
		return
	}
	go validator.Run(ctx, certFile, keyFile)
	klog.Infof("Webhook server running on port %d", port)
}

func waitForCertsReady(keyFile, CertFile string) bool {
	waitTimeout := 30 * time.Second
	waitInterval := 500 * time.Millisecond
	start := time.Now()
	for {
		if fileExists(CertFile) && fileExists(keyFile) {
			return true
		}
		if time.Since(start) > waitTimeout {
			klog.Warningf("timeout waiting for TLS cert/key files to appear at %s and %s", keyFile, CertFile)
			return false
		}
		time.Sleep(waitInterval)
	}
}

// getNamespace returns the current pod namespace or "default".
func getNamespace() string {
	return os.Getenv("POD_NAMESPACE")
}

// fileExists returns true if the file exists.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
