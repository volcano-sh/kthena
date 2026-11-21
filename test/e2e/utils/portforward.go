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

package utils

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/rest" // Required for *rest.Config type used by spdy.RoundTripperFor
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// forwardSession abstracts one forwarding session, allowing fakes in tests.
// *portforward.PortForwarder satisfies this interface.
type forwardSession interface {
	ForwardPorts() error
	GetPorts() ([]portforward.ForwardedPort, error)
}

var _ forwardSession = (*portforward.PortForwarder)(nil)

// PortForwarder manages the forwarding of a single port.
// This implementation is inspired by the Istio portforwarder:
// https://github.com/istio/istio/blob/master/pkg/kube/portforwarder.go
type PortForwarder interface {
	// Start runs this forwarder.
	Start() error

	// Close this forwarder and release any resources.
	Close()
}

var _ PortForwarder = &forwarder{}

const (
	reconnectInterval     = 2 * time.Second
	defaultStartupTimeout = 30 * time.Second
	maxPermanentStreak    = 3
)

type forwarder struct {
	stopCh    chan struct{}
	doneCh    chan struct{} // closed by run() when goroutine exits
	closeOnce sync.Once
	portMu    sync.Mutex // protects localPort during the Start() / run() handoff

	namespace    string
	podName      string // updated by run() on each reconnect
	localAddress string
	localPort    int
	podPort      int // updated by run() on each reconnect

	// resolveTarget re-resolves the forwarding target on each reconnect.
	// For pod-direct forwards it always returns (podName, podPort, nil).
	// For service-based forwards it queries the service's current running pod.
	resolveTarget func() (podName string, podPort int, err error)

	// buildFn constructs one forwarding session. Production code wraps
	// buildK8sPortForwarder; tests inject fakes.
	buildFn func(readyCh chan struct{}) (forwardSession, error)

	// reconnectWait overrides reconnectInterval in tests (0 uses the constant).
	reconnectWait time.Duration
	// startupTimeout overrides defaultStartupTimeout in tests (0 uses the constant).
	startupTimeout time.Duration
	// skipDialCheck disables the TCP reachability check in Start() for unit tests.
	skipDialCheck bool
}

func (f *forwarder) Start() error {
	if f.buildFn == nil {
		f.buildFn = func(readyCh chan struct{}) (forwardSession, error) {
			return f.buildK8sPortForwarder(readyCh)
		}
	}

	timeout := f.startupTimeout
	if timeout == 0 {
		timeout = defaultStartupTimeout
	}

	startErrCh := make(chan error, 1)
	// initFwCh carries the initial session from run() to Start() with an
	// explicit channel-based happens-before guarantee (Go memory model §channel).
	initFwCh := make(chan forwardSession, 1)
	readyCh := make(chan struct{}, 1)

	go f.run(startErrCh, initFwCh, readyCh)

	// Phase 1: wait for initial build result.
	var fw forwardSession
	select {
	case err := <-startErrCh:
		<-f.doneCh
		return fmt.Errorf("failure running port forward process: %v", err)
	case fw = <-initFwCh:
		// Build succeeded; fall through to wait for tunnel establishment.
	case <-f.doneCh:
		return fmt.Errorf("port-forward goroutine exited before initial build completed")
	}

	// Phase 2: wait for the port-forward library to signal that the local
	// listener is established. Only then is the tunnel usable.
	select {
	case <-readyCh:
		// Tunnel established.
	case <-f.doneCh:
		return fmt.Errorf("port-forward goroutine exited before tunnel was established")
	case <-time.After(timeout):
		f.closeOnce.Do(func() { close(f.stopCh) })
		<-f.doneCh
		return fmt.Errorf("timeout waiting for port-forward to become ready on %s", f.address())
	}

	// Phase 3: port discovery. fw was delivered via initFwCh (channel send
	// happens-before the receive here), so this access is race-free.
	p, err := fw.GetPorts()
	if err != nil {
		f.closeOnce.Do(func() { close(f.stopCh) })
		<-f.doneCh
		return fmt.Errorf("failed to get forwarded ports: %v", err)
	}
	if len(p) == 0 {
		f.closeOnce.Do(func() { close(f.stopCh) })
		<-f.doneCh
		return fmt.Errorf("port forwarder returned no ports")
	}
	// Set local port now; it may differ from input if 0 was given.
	// portMu guards the write so run()'s reconnect reads are race-free.
	f.portMu.Lock()
	f.localPort = int(p[0].Local)
	f.portMu.Unlock()

	if f.skipDialCheck {
		return nil
	}

	// Phase 4: TCP reachability check — only return success after the forwarded
	// address is actually accepting connections.
	address := f.address()
	deadline := time.Now().Add(15 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		conn, dialErr := net.DialTimeout("tcp", address, 1*time.Second)
		if dialErr == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			f.closeOnce.Do(func() { close(f.stopCh) })
			<-f.doneCh
			return fmt.Errorf("timeout waiting for port-forward on %s to become available", address)
		}
		select {
		case <-ticker.C:
		case <-f.doneCh:
			return fmt.Errorf("port-forward goroutine exited before %s became available", address)
		}
	}
}

// run is the reconnect loop. It runs in a dedicated goroutine and closes
// f.doneCh when it exits. After Start() returns, run() never sends to any
// startup channel — it only uses f.stopCh for shutdown.
func (f *forwarder) run(startErrCh chan<- error, initFwCh chan<- forwardSession, firstReadyCh chan struct{}) {
	defer close(f.doneCh)

	wait := f.reconnectWait
	if wait == 0 {
		wait = reconnectInterval
	}

	isFirst := true
	permanentStreak := 0

	for {
		select {
		case <-f.stopCh:
			return
		default:
		}

		// On reconnects, re-resolve the forwarding target so that a restarted
		// pod (with a new name) is picked up. Skip on the first run; the
		// caller already resolved and set the initial target.
		if !isFirst {
			podName, podPort, err := f.resolveTarget()
			if err != nil {
				// Target not yet available (pod restarting); always transient.
				select {
				case <-f.stopCh:
					return
				case <-time.After(wait):
				}
				continue
			}
			f.podName = podName
			f.podPort = podPort
		}

		readyCh := firstReadyCh
		if !isFirst {
			readyCh = nil
		}

		fw, err := f.buildFn(readyCh)
		if err != nil {
			if isFirst {
				// Initial build failure — report directly to Start().
				startErrCh <- fmt.Errorf("building port forwarder: %v", err)
				return
			}
			// Reconnect build failure. Classify and handle.
			if isTransientForwardError(err) {
				permanentStreak = 0
				select {
				case <-f.stopCh:
					return
				case <-time.After(wait):
				}
				continue
			}
			// Potentially permanent error (auth failure, bad config, …).
			permanentStreak++
			if permanentStreak >= maxPermanentStreak {
				fmt.Fprintf(os.Stderr,
					"port-forward %s/%s: %d consecutive non-transient errors, last: %v — stopping\n",
					f.namespace, f.podName, permanentStreak, err)
				return
			}
			select {
			case <-f.stopCh:
				return
			case <-time.After(wait):
			}
			continue
		}
		permanentStreak = 0

		// Deliver fw to Start() via initFwCh BEFORE calling ForwardPorts().
		// The channel send establishes the happens-before that makes Start()'s
		// subsequent fw.GetPorts() call race-free.
		if isFirst {
			initFwCh <- fw
		}
		isFirst = false

		// ForwardPorts blocks until the connection drops or stopCh is closed.
		// On the first run it also signals firstReadyCh (via the portforward
		// library) once the local listener is established, unblocking Phase 2.
		if err := fw.ForwardPorts(); err != nil {
			fmt.Fprintf(os.Stderr, "port-forward %s/%s: %v (will reconnect)\n",
				f.namespace, f.podName, err)
		}

		// Connection dropped; back off before the next reconnect attempt.
		select {
		case <-f.stopCh:
			return
		case <-time.After(wait):
		}
	}
}

// address returns the local forwarded address.
func (f *forwarder) address() string {
	return net.JoinHostPort(f.localAddress, strconv.Itoa(f.localPort))
}

// Close terminates the port-forward and blocks until the forwarding goroutine
// has exited. Close is idempotent; multiple calls are safe and do not panic.
func (f *forwarder) Close() {
	f.closeOnce.Do(func() { close(f.stopCh) })
	<-f.doneCh
}

// isTransientForwardError returns true for errors that indicate the pod is
// temporarily unavailable. All other build errors are treated as potentially
// permanent (retried up to maxPermanentStreak times before giving up).
func isTransientForwardError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "pod is not running") ||
		strings.Contains(msg, "pod is terminating") ||
		strings.Contains(msg, "no running pod found")
}

// newForwarder allocates a forwarder. resolve is called on each reconnect to
// obtain the current forwarding target; for pod-direct forwards it always
// returns the same (podName, podPort).
func newForwarder(namespace, podName string, localPort, podPort int, resolve func() (string, int, error)) *forwarder {
	return &forwarder{
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
		namespace:     namespace,
		podName:       podName,
		localAddress:  "127.0.0.1",
		localPort:     localPort,
		podPort:       podPort,
		resolveTarget: resolve,
	}
}

// AllocateLocalPortE binds to 127.0.0.1:0, returns the allocated port as a string, and
// releases the listener. Callers that do not have a *testing.T should use this variant.
func AllocateLocalPortE() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("allocate local port: %w", err)
	}
	port := fmt.Sprintf("%d", listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		return "", fmt.Errorf("release local port listener: %w", err)
	}
	return port, nil
}

// AllocateLocalPort binds to 127.0.0.1:0 and returns the allocated port as a string.
func AllocateLocalPort(t *testing.T) string {
	t.Helper()
	port, err := AllocateLocalPortE()
	require.NoError(t, err)
	return port
}

// SetupPortForward sets up a port-forward to a service and waits for it to be ready.
// It returns a PortForwarder interface that can be used to manage the port-forward.
// If SetupPortForward fails, the test should stop immediately as the error indicates
// a critical infrastructure issue that prevents the test from continuing.
func SetupPortForward(namespace, service, localPort, remotePort string) (PortForwarder, error) {
	config, err := GetKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %v", err)
	}

	remotePortIntOrStr := intstr.Parse(remotePort)
	podName, targetPort, err := findPodForService(clientset, namespace, service, remotePortIntOrStr)
	if err != nil {
		return nil, fmt.Errorf("failed to find pod for service %s/%s: %v", namespace, service, err)
	}
	localPortInt, err := strconv.Atoi(localPort)
	if err != nil {
		return nil, fmt.Errorf("invalid local port %q: %v", localPort, err)
	}

	// Capture clientset in the resolver so reconnects re-query the service
	// without constructing a new REST client each time.
	resolve := func() (string, int, error) {
		return findPodForService(clientset, namespace, service, remotePortIntOrStr)
	}

	f := newForwarder(namespace, podName, localPortInt, targetPort, resolve)
	if err := f.Start(); err != nil {
		return nil, fmt.Errorf("failed to start port-forward 127.0.0.1:%d -> %s/%s:%d: %v",
			localPortInt, namespace, service, targetPort, err)
	}
	return f, nil
}

// SetupPortForwardToPod sets up a port-forward directly to a pod and waits for it to be ready.
// It returns a PortForwarder interface that can be used to manage the port-forward.
// If SetupPortForwardToPod fails, the test should stop immediately as the error indicates
// a critical infrastructure issue that prevents the test from continuing.
func SetupPortForwardToPod(namespace, podName, localPort, podPort string) (PortForwarder, error) {
	localPortInt, err := strconv.Atoi(localPort)
	if err != nil {
		return nil, fmt.Errorf("invalid local port %q: %v", localPort, err)
	}
	podPortInt, err := strconv.Atoi(podPort)
	if err != nil {
		return nil, fmt.Errorf("invalid pod port %q: %v", podPort, err)
	}

	// Pod-direct: fixed target. The forwarder retries the same pod on reconnect;
	// if that pod is gone the reconnect will keep failing until Close() is called.
	resolve := func() (string, int, error) {
		return podName, podPortInt, nil
	}

	f := newForwarder(namespace, podName, localPortInt, podPortInt, resolve)
	if err := f.Start(); err != nil {
		return nil, fmt.Errorf("failed to start port-forward 127.0.0.1:%d -> %s/%s:%d: %v",
			localPortInt, namespace, podName, podPortInt, err)
	}
	return f, nil
}

// findPodForService finds a running pod for the given service and gets the targetPort
// from the service's port configuration. Returns pod name and the targetPort number.
func findPodForService(clientset *kubernetes.Clientset, namespace, serviceName string, servicePort intstr.IntOrString) (string, int, error) {
	ctx := context.Background()

	svc, err := clientset.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("failed to get service: %v", err)
	}

	var targetPort intstr.IntOrString
	found := false
	for _, port := range svc.Spec.Ports {
		if servicePort.Type == intstr.Int {
			if port.Port == int32(servicePort.IntValue()) {
				targetPort = port.TargetPort
				found = true
				break
			}
		} else {
			if port.Name == servicePort.StrVal {
				targetPort = port.TargetPort
				found = true
				break
			}
		}
	}
	if !found {
		return "", 0, fmt.Errorf("port %v not found in service %s/%s", servicePort, namespace, serviceName)
	}

	selector := metav1.FormatLabelSelector(&metav1.LabelSelector{
		MatchLabels: svc.Spec.Selector,
	})
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", 0, fmt.Errorf("failed to list pods: %v", err)
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodRunning && pod.DeletionTimestamp == nil {
			containerPort, err := resolveContainerPort(&pod, targetPort)
			if err != nil {
				continue
			}
			return pod.Name, containerPort, nil
		}
	}
	return "", 0, fmt.Errorf("no running pod found for service %s", serviceName)
}

// resolveContainerPort resolves the actual container port from targetPort.
// If targetPort is a number, it returns that number.
// If targetPort is a named port, it looks up the port name in the pod's container ports.
func resolveContainerPort(pod *v1.Pod, targetPort intstr.IntOrString) (int, error) {
	if targetPort.Type == intstr.Int {
		return targetPort.IntValue(), nil
	}
	portName := targetPort.StrVal
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name == portName {
				return int(port.ContainerPort), nil
			}
		}
	}
	return 0, fmt.Errorf("named port %q not found in pod %s/%s", portName, pod.Namespace, pod.Name)
}

// buildK8sPortForwarder builds a Kubernetes port forwarder
func (f *forwarder) buildK8sPortForwarder(readyCh chan struct{}) (*portforward.PortForwarder, error) {
	restConfig, err := GetKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %v", err)
	}

	restClient := clientset.CoreV1().RESTClient()
	req := restClient.Post().Resource("pods").Namespace(f.namespace).Name(f.podName).SubResource("portforward")
	serverURL := req.URL()

	roundTripper, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failure creating roundtripper: %v", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, serverURL)

	f.portMu.Lock()
	localPort := f.localPort
	f.portMu.Unlock()
	fw, err := portforward.NewOnAddresses(dialer,
		[]string{f.localAddress},
		[]string{fmt.Sprintf("%d:%d", localPort, f.podPort)},
		f.stopCh,
		readyCh,
		io.Discard,
		os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("failed establishing port-forward: %v", err)
	}

	// Pre-flight pod check: fail early rather than spinning on a pod that cannot
	// accept port-forwards. Same check as kubectl port-forward.
	podGet := restClient.Get().Resource("pods").Namespace(f.namespace).Name(f.podName)
	obj, err := podGet.Do(context.TODO()).Get()
	if err != nil {
		return nil, fmt.Errorf("failed retrieving: %v in the %q namespace", err, f.namespace)
	}
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return nil, fmt.Errorf("failed getting pod, object type is %T", obj)
	}
	if pod.Status.Phase != v1.PodRunning {
		return nil, fmt.Errorf("pod is not running. Status=%v", pod.Status.Phase)
	}
	if pod.DeletionTimestamp != nil {
		return nil, fmt.Errorf("pod %s/%s is terminating", pod.Namespace, pod.Name)
	}

	return fw, nil
}
