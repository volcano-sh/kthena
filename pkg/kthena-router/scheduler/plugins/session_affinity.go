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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

const SessionAffinityPluginName = "session-affinity"

const (
	defaultSessionAffinityHeaderName = "X-Session-ID"
	defaultSessionAffinityTTL        = 30 * time.Minute
	maxPluginScore                   = 100
)

var (
	_ framework.ScorePlugin      = &SessionAffinity{}
	_ framework.PostScheduleHook = &SessionAffinity{}
)

type SessionAffinityArgs struct {
	HeaderName string `yaml:"headerName,omitempty"`
	TTL        string `yaml:"ttl,omitempty"`
}

type sessionAffinityConfig struct {
	HeaderName string
	TTL        time.Duration
}

type affinityBinding struct {
	pod       types.NamespacedName
	expiresAt time.Time
}

type affinityStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	now      func() time.Time
	bindings map[string]affinityBinding
}

type SessionAffinity struct {
	name       string
	headerName string
	store      *affinityStore
}

func NewSessionAffinity(pluginArg runtime.RawExtension) *SessionAffinity {
	cfg := ParseSessionAffinityArgs(pluginArg)
	return &SessionAffinity{
		name:       SessionAffinityPluginName,
		headerName: cfg.HeaderName,
		store:      newAffinityStore(cfg.TTL),
	}
}

func ParseSessionAffinityArgs(pluginArg runtime.RawExtension) sessionAffinityConfig {
	args := SessionAffinityArgs{
		HeaderName: defaultSessionAffinityHeaderName,
		TTL:        defaultSessionAffinityTTL.String(),
	}

	if len(pluginArg.Raw) > 0 {
		if err := yaml.Unmarshal(pluginArg.Raw, &args); err != nil {
			klog.Errorf("Failed to unmarshal SessionAffinityArgs, using default values: %v", err)
			args = SessionAffinityArgs{
				HeaderName: defaultSessionAffinityHeaderName,
				TTL:        defaultSessionAffinityTTL.String(),
			}
		}
	}

	cfg := sessionAffinityConfig{
		HeaderName: defaultSessionAffinityHeaderName,
		TTL:        defaultSessionAffinityTTL,
	}
	if args.HeaderName != "" {
		cfg.HeaderName = args.HeaderName
	}
	if args.TTL != "" {
		ttl, err := time.ParseDuration(args.TTL)
		if err != nil || ttl <= 0 {
			klog.Errorf("Invalid session-affinity ttl %q, using default %s", args.TTL, defaultSessionAffinityTTL)
		} else {
			cfg.TTL = ttl
		}
	}

	return cfg
}

func (s *SessionAffinity) HeaderName() string {
	return s.headerName
}

func (s *SessionAffinity) Name() string {
	return s.name
}

func (s *SessionAffinity) Score(ctx *framework.Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int {
	scores := make(map[*datastore.PodInfo]int, len(pods))
	for _, pod := range pods {
		scores[pod] = 0
	}

	if ctx == nil || ctx.SessionKey == "" || ctx.AffinityScopeKey == "" || len(pods) == 0 {
		return scores
	}

	scopeKey := getScoreAffinityScopeKey(ctx)
	if scopeKey == "" {
		return scores
	}

	binding, ok := s.store.get(scopeKey, ctx.SessionKey)
	if !ok {
		return scores
	}

	for _, pod := range pods {
		if pod.Pod == nil {
			continue
		}
		if pod.Pod.Namespace == binding.Namespace && pod.Pod.Name == binding.Name {
			scores[pod] = maxPluginScore
			return scores
		}
	}

	s.store.delete(scopeKey, ctx.SessionKey)
	return scores
}

func (s *SessionAffinity) PostSchedule(ctx *framework.Context, index int) {
	if ctx == nil || ctx.SessionKey == "" || ctx.AffinityScopeKey == "" {
		return
	}

	if ctx.BestPods != nil {
		if index >= 0 && index < len(ctx.BestPods) {
			s.bindPod(ctx.AffinityScopeKey, ctx.SessionKey, ctx.BestPods[index])
		}
		return
	}

	if index >= 0 && index < len(ctx.DecodePods) {
		s.bindPod(ctx.AffinityScopeKey+"/decode", ctx.SessionKey, ctx.DecodePods[index])
	}
	if index >= 0 && index < len(ctx.PrefillPods) {
		s.bindPod(ctx.AffinityScopeKey+"/prefill", ctx.SessionKey, ctx.PrefillPods[index])
	}
}

func (s *SessionAffinity) bindPod(scopeKey string, sessionKey string, pod *datastore.PodInfo) {
	if scopeKey == "" || pod == nil || pod.Pod == nil {
		return
	}
	s.store.set(scopeKey, sessionKey, types.NamespacedName{
		Namespace: pod.Pod.Namespace,
		Name:      pod.Pod.Name,
	})
}

func getScoreAffinityScopeKey(ctx *framework.Context) string {
	if ctx == nil || ctx.AffinityScopeKey == "" {
		return ""
	}
	if ctx.PDGroup == nil {
		return ctx.AffinityScopeKey
	}
	if len(ctx.DecodePods) == 0 {
		return ctx.AffinityScopeKey + "/decode"
	}
	return ctx.AffinityScopeKey + "/prefill"
}

func newAffinityStore(ttl time.Duration) *affinityStore {
	return &affinityStore{
		ttl:      ttl,
		now:      time.Now,
		bindings: make(map[string]affinityBinding),
	}
}

func (s *affinityStore) get(scopeKey, sessionKey string) (types.NamespacedName, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := affinityBindingKey(scopeKey, sessionKey)
	binding, ok := s.bindings[key]
	if !ok {
		return types.NamespacedName{}, false
	}
	if !binding.expiresAt.After(s.now()) {
		delete(s.bindings, key)
		return types.NamespacedName{}, false
	}
	return binding.pod, true
}

func (s *affinityStore) set(scopeKey, sessionKey string, pod types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.bindings[affinityBindingKey(scopeKey, sessionKey)] = affinityBinding{
		pod:       pod,
		expiresAt: s.now().Add(s.ttl),
	}
}

func (s *affinityStore) delete(scopeKey, sessionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bindings, affinityBindingKey(scopeKey, sessionKey))
}

func affinityBindingKey(scopeKey, sessionKey string) string {
	return scopeKey + "\x00" + sessionKey
}
