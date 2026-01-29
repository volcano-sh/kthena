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
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	"github.com/volcano-sh/kthena/pkg/controller"
	modelboosterwebhook "github.com/volcano-sh/kthena/pkg/model-booster-controller/webhook"
	modelservingwebhook "github.com/volcano-sh/kthena/pkg/model-serving-controller/webhook"
	webhookcert "github.com/volcano-sh/kthena/pkg/webhook/cert"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

type webhookConfig struct {
	tlsCertFile    string
	tlsPrivateKey  string
	port           int
	webhookTimeout int
	certSecretName string
	serviceName    string
	kubeAPIQPS     float32
	kubeAPIBurst   int
}

func main() {
	var enableWebhook bool
	var wc webhookConfig
	var cc controller.Config
	var controllers []string
	// Initialize klog flags
	klog.InitFlags(nil)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.StringVar(&cc.Kubeconfig, "kubeconfig", "", "kubeconfig file path")
	pflag.BoolVar(&enableWebhook, "enable-webhook", true, "If true, webhook will be used. Default is true")
	pflag.StringVar(&cc.MasterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	pflag.StringVar(&wc.tlsCertFile, "tls-cert-file", "/etc/tls/tls.crt", "File containing the x509 Certificate for HTTPS")
	pflag.StringVar(&wc.tlsPrivateKey, "tls-private-key-file", "/etc/tls/tls.key", "File containing the x509 private key to --tls-cert-file")
	pflag.IntVar(&wc.port, "port", 8443, "Secure port that the webhook listens on")
	pflag.IntVar(&wc.webhookTimeout, "webhook-timeout", 30, "Timeout for webhook operations in seconds")
	pflag.StringVar(&wc.certSecretName, "cert-secret-name", "kthena-controller-manager-webhook-certs", "Name of the secret to store auto-generated certificates")
	pflag.StringVar(&wc.serviceName, "service-name", "kthena-controller-manager-webhook", "Service name for the webhook server")
	pflag.BoolVar(&cc.EnableLeaderElection, "leader-elect", false, "Enable leader election for controller. "+
		"Enabling this will ensure there is only one active controller. Default is false.")
	pflag.IntVar(&cc.Workers, "workers", 5, "number of workers to run. Default is 5")
	pflag.StringSliceVar(&controllers, "controllers", []string{"*"}, "A list of controllers to enable. '*' enables all controllers, 'foo' enables the controller "+
		"named 'foo', '-foo' disables the controller named 'foo'.\nIf both '+foo' and '-foo' are set simultaneously, then controller named 'foo' will be enabled.\nAll controllers: 'modelserving', 'modelbooster', 'autoscaler'")
	pflag.Float32Var(&cc.KubeAPIQPS, "kube-api-qps", 0, "QPS to use while talking with kubernetes apiserver. If 0, use default value.")
	pflag.IntVar(&cc.KubeAPIBurst, "kube-api-burst", 0, "Burst to use while talking with kubernetes apiserver. If 0, use default value.")
	pflag.Parse()

	cc.Controllers = parseControllers(controllers)

	pflag.CommandLine.VisitAll(func(f *pflag.Flag) {
		klog.Infof("Flag: %s, Value: %s", f.Name, f.Value.String())
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		klog.Info("Received termination, signaling shutdown")
		cancel()
	}()
	wc.kubeAPIQPS = cc.KubeAPIQPS
	wc.kubeAPIBurst = cc.KubeAPIBurst
	if enableWebhook {
		go func() {
			if err := setupWebhook(ctx, wc); err != nil {
				os.Exit(1)
			}
		}()
	}
	controller.SetupController(ctx, cc)
}

const validatingWebhookName = "kthena-controller-manager-validating-webhook"
const mutatingWebhookName = "kthena-controller-manager-mutating-webhook"

// ensureWebhookCertificate generates a certificate into the secret and returns the CA bundle.
func ensureWebhookCertificate(ctx context.Context, kubeClient kubernetes.Interface, wc webhookConfig) ([]byte, error) {
	namespace := getNamespace()
	dnsNames := []string{
		fmt.Sprintf("%s.%s.svc", wc.serviceName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", wc.serviceName, namespace),
	}
	klog.Infof("Auto-generating certificate for webhook server (secret=%s service=%s)", wc.certSecretName, wc.serviceName)
	return webhookcert.EnsureCertificate(ctx, kubeClient, namespace, wc.certSecretName, dnsNames)
}

func setupWebhook(ctx context.Context, wc webhookConfig) error {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("build client config: %v", err)
		return err
	}
	// Set QPS and Burst if provided
	if wc.kubeAPIQPS > 0 {
		cfg.QPS = wc.kubeAPIQPS
	}
	if wc.kubeAPIBurst > 0 {
		cfg.Burst = wc.kubeAPIBurst
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("failed to create kubeClient: %v", err)
		return err
	}

	kthenaClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("failed to create kthenaClient: %v", err)
		return err
	}

	// Secret -> File -> Generate precedence for CA bundle selection
	namespace := getNamespace()
	var caBundle []byte

	// 1. Try secret first.
	if bundle, err := webhookcert.LoadCertBundleFromSecret(ctx, kubeClient, namespace, wc.certSecretName); err != nil {
		klog.Warningf("Error reading CA bundle from secret %s: %v", wc.certSecretName, err)
	} else if bundle != nil {
		klog.Infof("Loaded CA bundle from secret %s", wc.certSecretName)
		caBundle = bundle.CAPEM
	}

	// 2. If not from secret, try existing cert file.
	if caBundle == nil {
		if !fileExists(wc.tlsPrivateKey) || !fileExists(wc.tlsCertFile) {
			b, err := ensureWebhookCertificate(ctx, kubeClient, wc)
			if err != nil {
				klog.Fatalf("Failed to auto-generate webhook certificates: %v", err)
			}
			caBundle = b
		}
	}

	if caBundle != nil {
		// Always update both webhook configurations with the chosen CA bundle
		if err := webhookcert.UpdateValidatingWebhookCABundle(ctx, kubeClient, validatingWebhookName, caBundle); err != nil {
			klog.Warningf("Failed to update ValidatingWebhookConfiguration CA bundle: %v", err)
		}
		if err := webhookcert.UpdateMutatingWebhookCABundle(ctx, kubeClient, mutatingWebhookName, caBundle); err != nil {
			klog.Warningf("Failed to update MutatingWebhookConfiguration CA bundle: %v", err)
		}
	}

	mux := http.NewServeMux()

	modelServingValidator := modelservingwebhook.NewModelServingValidator()
	mux.HandleFunc("/validate-workload-ai-v1alpha1-modelserving", modelServingValidator.Handle)

	modelValidator := modelboosterwebhook.NewModelValidator()
	modelMutator := modelboosterwebhook.NewModelMutator()
	autoscalingPolicyValidator := modelboosterwebhook.NewAutoscalingPolicyValidator()
	autoscalingPolicyMutator := modelboosterwebhook.NewAutoscalingPolicyMutator()
	autoscalingBindingValidator := modelboosterwebhook.NewAutoscalingBindingValidator(kthenaClient)
	mux.HandleFunc("/validate/modelbooster", modelValidator.Handle)
	mux.HandleFunc("/mutate/modelbooster", modelMutator.Handle)
	mux.HandleFunc("/validate/autoscalingpolicy", autoscalingPolicyValidator.Handle)
	mux.HandleFunc("/mutate/autoscalingpolicy", autoscalingPolicyMutator.Handle)
	mux.HandleFunc("/validate/autoscalingpolicybinding", autoscalingBindingValidator.Handle)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			klog.Errorf("failed to write health check response: %v", err)
		}
	})

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", wc.port),
		Handler:      mux,
		ReadTimeout:  time.Duration(wc.webhookTimeout) * time.Second,
		WriteTimeout: time.Duration(wc.webhookTimeout) * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	// Wait for both cert and key files to exist (in case they are mounted by Kubernetes)
	ok := waitForCertsReady(wc.tlsPrivateKey, wc.tlsCertFile)
	if !ok {
		return fmt.Errorf("TLS cert/key files not found, webhook server cannot start")
	}

	go func() {
		klog.Infof("Starting webhook server on %s", server.Addr)
		if err := server.ListenAndServeTLS(wc.tlsCertFile, wc.tlsPrivateKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Fatalf("failed to start unified webhook server: %v", err)
		}
	}()
	<-ctx.Done()
	ctxTimeout, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctxTimeout)
	return nil
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

func parseControllers(controllers []string) map[string]bool {
	// defaultControllers defines all available controllers as enabled
	defaultControllers := map[string]bool{
		controller.ModelServingController: true,
		controller.ModelBoosterController: true,
		controller.AutoscalerController:   true,
	}

	enableControllers := make(map[string]bool)

	for i := range controllers {
		controllers[i] = strings.TrimSpace(controllers[i])
		if controllers[i] == "" {
			continue
		}
	}

	for ctrlName := range defaultControllers {
		// Determine if the controller should be enabled based on user input
		if isControllerEnabled(ctrlName, controllers) {
			enableControllers[ctrlName] = true
		} else {
			klog.Infof("Controller <%s> is disabled", ctrlName)
		}
	}

	if len(enableControllers) == 0 {
		klog.Warning("No controllers are enabled")
		return defaultControllers
	}

	return enableControllers
}

// isControllerEnabled check if a specified controller enabled or not.
// If the input controllers starts with a "+name" or "name", it is considered as an explicit inclusion.
// Otherwise, it is considered as an explicit exclusion.
func isControllerEnabled(name string, controllers []string) bool {
	// Because controllers are enabled by default, the default return value is true.
	hasStar := false
	// if no explicit inclusion or exclusion, enable all controllers by default
	if len(controllers) == 0 {
		return true
	}
	for _, ctrl := range controllers {
		// if we get here, there was an explicit inclusion
		if ctrl == name {
			return true
		}
		// if we get here, there was an explicit inclusion
		if ctrl == "+"+name {
			return true
		}
		// if we get here, there was an explicit exclusion
		if ctrl == "-"+name {
			return false
		}
		if ctrl == "*" {
			hasStar = true
		}
	}
	// if we get here, there was no explicit inclusion or exclusion
	return hasStar
}
