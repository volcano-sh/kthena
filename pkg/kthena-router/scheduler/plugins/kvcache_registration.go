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

package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

const (
	runtimeRegisterPath = "/kvcache/routers/register"

	defaultRuntimePort             = 9000
	defaultRegistrationIntervalSec = 30
	defaultRegistrationTTLSec      = 90

	// maxConcurrentRegistrations bounds the fan-out of one registration sweep
	// so that many unreachable pods cannot stretch a sweep past the TTL.
	maxConcurrentRegistrations = 16
)

// routerRegistrationRequest is the body posted to the runtime sidecar's
// /kvcache/routers/register endpoint. Kept in sync by hand with
// python/kthena/runtime/app.py.
type routerRegistrationRequest struct {
	RouterID string `json:"router_id"`
	Endpoint string `json:"endpoint"`
	// Generation identifies one router process. It changes when the router
	// container restarts (even with an unchanged pod name), so sidecars can
	// detect that the in-memory index was lost and push a fresh snapshot.
	Generation string `json:"generation"`
	TTLSeconds int    `json:"ttl_seconds"`
}

// kvRouterRegistrar periodically registers this router instance with the
// runtime sidecar of every known model-serving pod, so the sidecars can push
// KV cache events back. Registration doubles as a heartbeat: sidecars expire
// routers that stop re-registering within the TTL.
type kvRouterRegistrar struct {
	store       datastore.Store
	routerID    string
	generation  string // unique per router process, see routerRegistrationRequest
	endpoint    string // base URL the sidecar pushes events to, e.g. http://10.0.0.5:9080
	runtimePort int
	interval    time.Duration
	ttlSeconds  int
	client      *http.Client
}

func newKVRouterRegistrar(store datastore.Store, routerID, endpoint string,
	runtimePort, intervalSeconds, ttlSeconds int) *kvRouterRegistrar {
	return &kvRouterRegistrar{
		store:       store,
		routerID:    routerID,
		generation:  newProcessGeneration(),
		endpoint:    endpoint,
		runtimePort: runtimePort,
		interval:    time.Duration(intervalSeconds) * time.Second,
		ttlSeconds:  ttlSeconds,
		client:      &http.Client{Timeout: 3 * time.Second},
	}
}

// newProcessGeneration returns an identifier that is unique per router
// process, so a restarted container with the same pod name still triggers a
// snapshot from every sidecar.
func newProcessGeneration() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}

// routerSelfEndpoint derives the base URL runtime sidecars should push KV
// events to. POD_IP must be injected via the downward API on the router
// deployment; without it memory mode cannot receive pushes.
func routerSelfEndpoint(eventsPort int) (string, error) {
	podIP := os.Getenv("POD_IP")
	if podIP == "" {
		return "", fmt.Errorf("POD_IP environment variable is not set")
	}
	return "http://" + net.JoinHostPort(podIP, strconv.Itoa(eventsPort)), nil
}

// routerInstanceID identifies this router replica in sidecar registries.
func routerInstanceID() string {
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "kthena-router"
	}
	return hostname
}

// run registers with all known pods immediately and then on every tick until
// ctx is done.
func (r *kvRouterRegistrar) run(ctx context.Context) {
	r.registerAll(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.registerAll(ctx)
		}
	}
}

func (r *kvRouterRegistrar) registerAll(ctx context.Context) {
	pods := r.store.GetAllPods()
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		registered int
	)
	// Bounded fan-out: registrations run concurrently so a sweep over many
	// pods without a reachable sidecar stays well below the TTL.
	sem := make(chan struct{}, maxConcurrentRegistrations)
	for _, podInfo := range pods {
		pod := podInfo.GetPod()
		if pod == nil || pod.Status.PodIP == "" || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(namespace, name, podIP string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := r.registerWithPod(ctx, podIP); err != nil {
				// Pods without a runtime sidecar (or not in memory mode) are expected
				// to fail; keep this quiet.
				klog.V(4).Infof("KVCacheAware: registration with pod %s/%s (%s) failed: %v",
					namespace, name, podIP, err)
				return
			}
			mu.Lock()
			registered++
			mu.Unlock()
		}(pod.Namespace, pod.Name, pod.Status.PodIP)
	}
	wg.Wait()
	klog.V(4).Infof("KVCacheAware: registered with %d/%d runtime sidecars", registered, len(pods))
}

func (r *kvRouterRegistrar) registerWithPod(ctx context.Context, podIP string) error {
	body, err := json.Marshal(routerRegistrationRequest{
		RouterID:   r.routerID,
		Endpoint:   r.endpoint,
		Generation: r.generation,
		TTLSeconds: r.ttlSeconds,
	})
	if err != nil {
		return err
	}

	url := "http://" + net.JoinHostPort(podIP, strconv.Itoa(r.runtimePort)) + runtimeRegisterPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}
