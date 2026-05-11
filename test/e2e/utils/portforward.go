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
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/rest" // Required for *rest.Config type used by spdy.RoundTripperFor
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

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

type forwarder struct {
	stopCh       chan struct{}
	podName      string
	namespace    string
	localAddress string
	localPort    int
	podPort      int
	errCh        chan error
}

func (f *forwarder) Start() error {
	f.errCh = make(chan error, 1)
	readyCh := make(chan struct{}, 1)

	var fw *portforward.PortForwarder
	go func() {
		for {
			select {
			case <-f.stopCh:
				return
			default:
			}
			var err error
			// Build a new port forwarder.
			fw, err = f.buildK8sPortForwarder(readyCh)
			if err != nil {
				f.errCh <- fmt.Errorf("building port forwarder: %v", err)
				return
			}
			if err = fw.ForwardPorts(); err != nil {
				f.errCh <- fmt.Errorf("port forward: %v", err)
				return
			}
			f.errCh <- nil
			// At this point, either the stopCh has been closed, or port forwarder connection is broken.
			// the port forwarder should have already been ready before.
			// No need to notify the ready channel anymore when forwarding again.
			readyCh = nil
		}
	}()

	// We want to block Start() until we have either gotten an error or have started
	// We may later get an error, but that is handled async.
	select {
	case err := <-f.errCh:
		return fmt.Errorf("failure running port forward process: %v", err)
	case <-readyCh:
		p, err := fw.GetPorts()
		if err != nil {
			return fmt.Errorf("failed to get ports: %v", err)
		}
		if len(p) == 0 {
			return fmt.Errorf("got no ports")
		}
		// Set local port now, as it may have been 0 as input
		f.localPort = int(p[0].Local)

		// Wait for port-forward to be accessible
		address := f.address()
		timeout := 15 * time.Second
		interval := 1 * time.Second
		start := time.Now()

		for {
			conn, err := net.DialTimeout("tcp", address, 1*time.Second)
			if err == nil {
				conn.Close()
				return nil
			}
			if time.Since(start) > timeout {
				return fmt.Errorf("timeout waiting for port-forward on %s to become available", address)
			}
			time.Sleep(interval)
		}
	}
}

// address returns the local forwarded address. This is a private method used internally.
func (f *forwarder) address() string {
	return net.JoinHostPort(f.localAddress, strconv.Itoa(f.localPort))
}

func (f *forwarder) Close() {
	close(f.stopCh)
	// Closing the stop channel should close anything
	// opened by f.forwarder.ForwardPorts()
}

// startForwarder creates and starts a port forwarder with the given parameters.
// This is a private helper function to avoid code duplication between SetupPortForward
// and SetupPortForwardToPod.
func startForwarder(namespace, podName string, localPort, podPort int) (PortForwarder, error) {
	f := &forwarder{
		stopCh:       make(chan struct{}),
		podName:      podName,
		namespace:    namespace,
		localAddress: "127.0.0.1",
		localPort:    localPort,
		podPort:      podPort,
	}

	// Start the forwarder - this is a critical step, failure here means the test cannot proceed
	if err := f.Start(); err != nil {
		return nil, fmt.Errorf("failed to start port-forward %s:%d -> %s/%s:%d: %v",
			f.localAddress, localPort, namespace, podName, podPort, err)
	}

	return f, nil
}

// SetupPortForward sets up a port-forward to a service and waits for it to be ready.
// It returns a PortForwarder interface that can be used to manage the port-forward.
// If SetupPortForward fails, the test should stop immediately as the error indicates
// a critical infrastructure issue that prevents the test from continuing.
func SetupPortForward(namespace, service, localPort, remotePort string) (PortForwarder, error) {
	// Get Kubernetes config
	config, err := GetKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %v", err)
	}

	// Create Kubernetes client
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %v", err)
	}

	// Parse remote port (service port)
	remotePortIntOrStr := intstr.Parse(remotePort)

	// Find a pod for the service and get the targetPort from service configuration
	podName, targetPort, err := findPodForService(clientset, namespace, service, remotePortIntOrStr)
	if err != nil {
		return nil, fmt.Errorf("failed to find pod for service %s/%s: %v", namespace, service, err)
	}

	// Parse local port
	localPortInt, err := strconv.Atoi(localPort)
	if err != nil {
		return nil, fmt.Errorf("invalid local port %q: %v", localPort, err)
	}

	return startForwarder(namespace, podName, localPortInt, targetPort)
}

// SetupPortForwardToPod sets up a port-forward directly to a pod and waits for it to be ready.
// It returns a PortForwarder interface that can be used to manage the port-forward.
// If SetupPortForwardToPod fails, the test should stop immediately as the error indicates
// a critical infrastructure issue that prevents the test from continuing.
func SetupPortForwardToPod(namespace, podName, localPort, podPort string) (PortForwarder, error) {
	// Parse local port
	localPortInt, err := strconv.Atoi(localPort)
	if err != nil {
		return nil, fmt.Errorf("invalid local port %q: %v", localPort, err)
	}

	// Parse pod port
	podPortInt, err := strconv.Atoi(podPort)
	if err != nil {
		return nil, fmt.Errorf("invalid pod port %q: %v", podPort, err)
	}

	return startForwarder(namespace, podName, localPortInt, podPortInt)
}

// findPodForService finds a running pod for the given service and gets the targetPort
// from the service's port configuration. Returns pod name and the targetPort number.
func findPodForService(clientset *kubernetes.Clientset, namespace, serviceName string, servicePort intstr.IntOrString) (string, int, error) {
	ctx := context.Background()

	// Get the service
	svc, err := clientset.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("failed to get service: %v", err)
	}

	// Find the targetPort from service port configuration
	var targetPort intstr.IntOrString
	found := false
	for _, port := range svc.Spec.Ports {
		// Match by port number or name
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

	// Get pods matching the service selector
	selector := metav1.FormatLabelSelector(&metav1.LabelSelector{
		MatchLabels: svc.Spec.Selector,
	})

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", 0, fmt.Errorf("failed to list pods: %v", err)
	}

	// Find the first running pod and resolve the targetPort
	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodRunning && pod.DeletionTimestamp == nil {
			// Resolve the container port from targetPort
			containerPort, err := resolveContainerPort(&pod, targetPort)
			if err != nil {
				continue // Try next pod if we can't resolve the port
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
	// If targetPort is already a number, return it
	if targetPort.Type == intstr.Int {
		return targetPort.IntValue(), nil
	}

	// If targetPort is a named port, find it in the pod's container ports
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
	// Get Kubernetes config
	restConfig, err := GetKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %v", err)
	}

	// Create Kubernetes clientset to get REST client
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %v", err)
	}

	// Use the REST client from CoreV1() which is already configured for core/v1 API
	restClient := clientset.CoreV1().RESTClient()

	req := restClient.Post().Resource("pods").Namespace(f.namespace).Name(f.podName).SubResource("portforward")
	serverURL := req.URL()

	roundTripper, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failure creating roundtripper: %v", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, serverURL)

	fw, err := portforward.NewOnAddresses(dialer,
		[]string{f.localAddress},
		[]string{fmt.Sprintf("%d:%d", f.localPort, f.podPort)},
		f.stopCh,
		readyCh,
		io.Discard,
		os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("failed establishing port-forward: %v", err)
	}

	// Run the same check as k8s.io/kubectl/pkg/cmd/portforward/portforward.go
	// so that we will fail early if there is a problem contacting API server.
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
