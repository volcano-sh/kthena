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

// Package filesource loads kthena router resources from a local directory
// instead of the Kubernetes API server. The files hold exactly the same
// manifests that would otherwise be applied to a cluster, which allows running
// the router standalone, without an API server, while keeping the ModelRoute,
// ModelServer and ExternalModelProvider APIs unchanged.
package filesource

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/klog/v2"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/controller"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
	"github.com/volcano-sh/kthena/pkg/kthena-router/webhook"
)

// DefaultNamespace is used for resources whose manifest omits metadata.namespace.
const DefaultNamespace = "default"

// defaultSyncPeriod is how often the directory is re-read when the caller does
// not configure a period.
const defaultSyncPeriod = 10 * time.Second

// resources holds one parsed snapshot of the resource directory.
type resources struct {
	modelRoutes  map[string]*aiv1alpha1.ModelRoute
	modelServers map[types.NamespacedName]*aiv1alpha1.ModelServer
	providers    map[types.NamespacedName]*aiv1alpha1.ExternalModelProvider
	secrets      map[types.NamespacedName]*corev1.Secret
}

func newResources() *resources {
	return &resources{
		modelRoutes:  make(map[string]*aiv1alpha1.ModelRoute),
		modelServers: make(map[types.NamespacedName]*aiv1alpha1.ModelServer),
		providers:    make(map[types.NamespacedName]*aiv1alpha1.ExternalModelProvider),
		secrets:      make(map[types.NamespacedName]*corev1.Secret),
	}
}

// Source watches a directory of manifests and keeps the datastore in sync with
// it. It implements the same contract as the API server backed controllers.
type Source struct {
	dir    string
	period time.Duration
	store  datastore.Store

	synced atomic.Bool
	// digest of the last successfully applied snapshot, used to skip re-parsing
	// unchanged directories.
	digest string
	// applied is the snapshot currently reflected in the store.
	applied *resources
}

// New creates a Source reading manifests from dir. A non-positive period falls
// back to the default sync period.
func New(dir string, period time.Duration, store datastore.Store) (*Source, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read resource directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("resource path %s is not a directory", dir)
	}
	if period <= 0 {
		period = defaultSyncPeriod
	}
	return &Source{
		dir:     dir,
		period:  period,
		store:   store,
		applied: newResources(),
	}, nil
}

// HasSynced reports whether the initial load completed.
func (s *Source) HasSynced() bool {
	return s.synced.Load()
}

// Run performs the initial load and then re-reads the directory periodically so
// that manifest changes are picked up without restarting the router.
func (s *Source) Run(stop <-chan struct{}) error {
	if err := s.sync(); err != nil {
		return err
	}
	s.synced.Store(true)

	go wait.Until(func() {
		if err := s.sync(); err != nil {
			klog.Errorf("failed to reload resources from %s: %v", s.dir, err)
		}
	}, s.period, stop)

	<-stop
	return nil
}

// sync reads the directory and applies the difference to the store.
func (s *Source) sync() error {
	files, err := s.listFiles()
	if err != nil {
		return err
	}

	digest, contents, err := readAll(files)
	if err != nil {
		return err
	}
	if digest == s.digest {
		return nil
	}

	next := newResources()
	for _, content := range contents {
		if err := decodeInto(content.path, content.data, next); err != nil {
			// Keep serving the last good snapshot instead of dropping resources
			// because of a half-written, malformed or invalid file.
			return err
		}
	}

	s.apply(next)
	s.digest = digest
	s.applied = next
	return nil
}

type fileContent struct {
	path string
	data []byte
}

// listFiles returns the manifest files directly inside the resource directory,
// sorted by name. Sub-directories are skipped so that manifests mounted from a
// ConfigMap are not read twice through the "..data" symlink.
func (s *Source) listFiles() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("failed to list resource directory %s: %w", s.dir, err)
	}

	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !isManifest(name) {
			continue
		}
		path := filepath.Join(s.dir, name)
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("failed to stat %s: %w", path, err)
		}
		if info.IsDir() {
			continue
		}
		files = append(files, path)
	}
	sort.Strings(files)
	return files, nil
}

func isManifest(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml", ".json":
		return true
	default:
		return false
	}
}

// readAll reads every file and returns a digest of the whole snapshot.
func readAll(files []string) (string, []fileContent, error) {
	hash := sha256.New()
	contents := make([]fileContent, 0, len(files))
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("failed to read %s: %w", path, err)
		}
		hash.Write([]byte(path))
		hash.Write(data)
		contents = append(contents, fileContent{path: path, data: data})
	}
	return hex.EncodeToString(hash.Sum(nil)), contents, nil
}

// decodeInto parses every document of a manifest file into res.
func decodeInto(path string, data []byte, res *resources) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for i := 0; ; i++ {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("failed to parse %s: %w", path, err)
		}
		if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}

		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			return fmt.Errorf("failed to parse %s document %d: %w", path, i, err)
		}

		if err := decodeObject(raw, typeMeta, res); err != nil {
			return fmt.Errorf("%s document %d: %w", path, i, err)
		}
	}
}

func decodeObject(raw json.RawMessage, typeMeta metav1.TypeMeta, res *resources) error {
	switch {
	case typeMeta.APIVersion == aiv1alpha1.SchemeGroupVersion.String() && typeMeta.Kind == aiv1alpha1.ModelRouteKind:
		obj := &aiv1alpha1.ModelRoute{}
		if err := unmarshal(raw, obj, &obj.ObjectMeta); err != nil {
			return err
		}
		// The admission webhook is disabled without an API server, so the same
		// semantic validation runs when the manifests are loaded.
		if ok, reason := webhook.ValidateModelRoute(obj); !ok {
			return fmt.Errorf("invalid ModelRoute %s/%s: %s", obj.Namespace, obj.Name, reason)
		}
		res.modelRoutes[obj.Namespace+"/"+obj.Name] = obj
	case typeMeta.APIVersion == aiv1alpha1.SchemeGroupVersion.String() && typeMeta.Kind == aiv1alpha1.ModelServerKind:
		obj := &aiv1alpha1.ModelServer{}
		if err := unmarshal(raw, obj, &obj.ObjectMeta); err != nil {
			return err
		}
		if ok, reason := webhook.ValidateModelServer(obj); !ok {
			return fmt.Errorf("invalid ModelServer %s/%s: %s", obj.Namespace, obj.Name, reason)
		}
		res.modelServers[utils.GetNamespaceName(obj)] = obj
	case typeMeta.APIVersion == aiv1alpha1.SchemeGroupVersion.String() && typeMeta.Kind == aiv1alpha1.ExternalModelProviderKind:
		obj := &aiv1alpha1.ExternalModelProvider{}
		if err := unmarshal(raw, obj, &obj.ObjectMeta); err != nil {
			return err
		}
		if ok, reason := webhook.ValidateExternalModelProvider(obj); !ok {
			return fmt.Errorf("invalid ExternalModelProvider %s/%s: %s", obj.Namespace, obj.Name, reason)
		}
		res.providers[utils.GetNamespaceName(obj)] = obj
	case typeMeta.APIVersion == "v1" && typeMeta.Kind == "Secret":
		obj := &corev1.Secret{}
		if err := unmarshal(raw, obj, &obj.ObjectMeta); err != nil {
			return err
		}
		// The API server converts `stringData` into `data` on write; without an
		// API server the conversion happens here. `stringData` wins on conflicts,
		// matching the Kubernetes semantics.
		if len(obj.StringData) > 0 {
			if obj.Data == nil {
				obj.Data = make(map[string][]byte, len(obj.StringData))
			}
			for key, value := range obj.StringData {
				obj.Data[key] = []byte(value)
			}
			obj.StringData = nil
		}
		res.secrets[utils.GetNamespaceName(obj)] = obj
	default:
		klog.Warningf("ignoring unsupported resource %s/%s", typeMeta.APIVersion, typeMeta.Kind)
	}
	return nil
}

func unmarshal(raw json.RawMessage, obj any, meta *metav1.ObjectMeta) error {
	if err := json.Unmarshal(raw, obj); err != nil {
		return err
	}
	if meta.Name == "" {
		return errors.New("metadata.name must be set")
	}
	if meta.Namespace == "" {
		meta.Namespace = DefaultNamespace
	}
	return nil
}

// apply pushes the new snapshot into the store and removes resources that are
// no longer present in the directory.
func (s *Source) apply(next *resources) {
	for key, mr := range next.modelRoutes {
		if err := s.store.AddOrUpdateModelRoute(mr); err != nil {
			klog.Errorf("failed to store model route %s: %v", key, err)
		}
	}
	for key := range s.applied.modelRoutes {
		if _, ok := next.modelRoutes[key]; ok {
			continue
		}
		if err := s.store.DeleteModelRoute(key); err != nil {
			klog.Errorf("failed to delete model route %s: %v", key, err)
		}
	}

	for name, secret := range next.secrets {
		if err := s.store.AddOrUpdateSecret(secret); err != nil {
			klog.Errorf("failed to store secret %s: %v", name, err)
		}
	}
	for name := range s.applied.secrets {
		if _, ok := next.secrets[name]; ok {
			continue
		}
		if err := s.store.DeleteSecret(name); err != nil {
			klog.Errorf("failed to delete secret %s: %v", name, err)
		}
	}

	for name, provider := range next.providers {
		if err := s.store.AddOrUpdateExternalModelProvider(provider); err != nil {
			klog.Errorf("failed to store external model provider %s: %v", name, err)
		}
	}
	for name := range s.applied.providers {
		if _, ok := next.providers[name]; ok {
			continue
		}
		if err := s.store.DeleteExternalModelProvider(name); err != nil {
			klog.Errorf("failed to delete external model provider %s: %v", name, err)
		}
	}

	for name, ms := range next.modelServers {
		if len(ms.Spec.Endpoints) == 0 {
			klog.Warningf("model server %s declares no endpoints; serving instances cannot be "+
				"discovered without the Kubernetes API server", name)
		}
		if err := controller.SyncStaticEndpoints(s.store, ms); err != nil {
			klog.Errorf("failed to store model server %s: %v", name, err)
		}
	}
	for name := range s.applied.modelServers {
		if _, ok := next.modelServers[name]; ok {
			continue
		}
		if err := s.store.DeleteModelServer(name); err != nil {
			klog.Errorf("failed to delete model server %s: %v", name, err)
		}
	}
}
