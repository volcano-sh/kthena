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

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestRecoveryBackoffConfigDelay(t *testing.T) {
	config := DefaultRecoveryBackoffConfig()
	tests := []struct {
		name     string
		config   RecoveryBackoffConfig
		failures int32
		want     time.Duration
	}{
		{name: "first recovery is immediate", config: config, failures: 0, want: 0},
		{name: "second recovery waits base", config: config, failures: 1, want: 10 * time.Second},
		{name: "delay doubles", config: config, failures: 2, want: 20 * time.Second},
		{name: "delay keeps doubling", config: config, failures: 5, want: 160 * time.Second},
		{name: "delay stops at max", config: config, failures: 6, want: 5 * time.Minute},
		{name: "large counts stay at max", config: config, failures: maxCountedFailures, want: 5 * time.Minute},
		{name: "zero base disables the backoff", config: RecoveryBackoffConfig{Max: time.Minute}, failures: 3, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.config.delay(tt.failures))
		})
	}
}

func TestRecoveryBackoffConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  RecoveryBackoffConfig
		wantErr bool
	}{
		{name: "defaults", config: DefaultRecoveryBackoffConfig()},
		{name: "disabled", config: RecoveryBackoffConfig{}},
		{name: "max equal to base", config: RecoveryBackoffConfig{Base: time.Minute, Max: time.Minute}},
		{name: "negative base", config: RecoveryBackoffConfig{Base: -time.Second, Max: time.Minute}, wantErr: true},
		{name: "negative reset", config: RecoveryBackoffConfig{Base: time.Second, Max: time.Minute, ResetAfter: -time.Second}, wantErr: true},
		{name: "max below base", config: RecoveryBackoffConfig{Base: time.Minute, Max: time.Second}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRecoveryBackoffStart(t *testing.T) {
	key := recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0", roleName: "prefill", roleID: "prefill-0"}
	tests := []struct {
		name         string
		failures     int32
		identity     string
		runningSince time.Duration // how long ago the unit became Running; 0 means it did not
		grace        time.Duration
		disrupted    bool
		wantWait     time.Duration
		wantFailures int32
		wantStarted  bool
	}{
		{name: "first failure uses the grace period", grace: 5 * time.Second, wantWait: 5 * time.Second},
		{name: "repeated failure waits for the backoff", failures: 2, wantWait: 20 * time.Second, wantFailures: 2, wantStarted: true},
		{name: "longer grace period wins", failures: 1, grace: time.Minute, wantWait: time.Minute, wantFailures: 1},
		{name: "disruption keeps the grace period", failures: 3, grace: time.Second, disrupted: true, wantWait: time.Second, wantFailures: 3},
		{name: "new revision resets the count", failures: 4, identity: "rev-2/hash", wantWait: 0},
		{name: "long healthy run resets the count", failures: 4, runningSince: 10 * time.Minute, wantWait: 0},
		{name: "short healthy run keeps the count", failures: 4, runningSince: 9 * time.Minute, wantWait: 80 * time.Second, wantFailures: 4, wantStarted: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
			b := newRecoveryBackoff(DefaultRecoveryBackoffConfig(), clocktesting.NewFakeClock(now))
			rec := &recoveryRecord{failures: tt.failures, identity: "rev-1/hash"}
			if tt.runningSince > 0 {
				rec.runningSince = now.Add(-tt.runningSince)
			}
			b.records[key] = rec
			identity := tt.identity
			if identity == "" {
				identity = "rev-1/hash"
			}

			wait, failures, started := b.start(key, identity, tt.grace, tt.disrupted)

			assert.Equal(t, tt.wantWait, wait)
			assert.Equal(t, tt.wantFailures, failures)
			assert.Equal(t, tt.wantStarted, started)
			assert.True(t, rec.runningSince.IsZero(), "a failed unit is no longer running")
			assert.Equal(t, tt.wantStarted, !rec.recoverAt.IsZero())
		})
	}
}

func TestRecoveryBackoffPodsOfOneUnitShareTheRecoveryTime(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Now())
	b := newRecoveryBackoff(DefaultRecoveryBackoffConfig(), clk)
	key := recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0"}
	b.records[key] = &recoveryRecord{failures: 2, identity: "rev-1"}

	wait, _, started := b.start(key, "rev-1", 0, false)
	require.True(t, started)
	require.Equal(t, 20*time.Second, wait)

	clk.Step(5 * time.Second)
	wait, _, started = b.start(key, "rev-1", 0, false)
	assert.False(t, started, "the unit is already in backoff")
	assert.Equal(t, 15*time.Second, wait)
}

func TestRecoveryBackoffFinish(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Now())
	b := newRecoveryBackoff(DefaultRecoveryBackoffConfig(), clk)
	key := recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0"}
	b.records[key] = &recoveryRecord{failures: 1, identity: "rev-1"}
	_, _, started := b.start(key, "rev-1", 0, false)
	require.True(t, started)

	assert.False(t, b.finish(key), "a timer from an earlier round must not clear a pending recovery")
	require.Len(t, b.pending("uid"), 1)

	clk.Step(10 * time.Second)
	assert.True(t, b.finish(key))
	assert.Empty(t, b.pending("uid"))
	assert.False(t, b.finish(key))
}

func TestRecoveryBackoffRecordFailureAndMarkRunning(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Now())
	b := newRecoveryBackoff(DefaultRecoveryBackoffConfig(), clk)
	known := recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0"}
	unknown := recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-1"}
	b.records[known] = &recoveryRecord{failures: maxCountedFailures - 1}

	b.recordFailure(known)
	b.recordFailure(known)
	b.recordFailure(unknown)
	b.markRunning(unknown)
	assert.Equal(t, int32(maxCountedFailures), b.records[known].failures)
	assert.NotContains(t, b.records, unknown, "healthy units without a failure history are not tracked")

	b.markRunning(known)
	first := b.records[known].runningSince
	clk.Step(time.Minute)
	b.markRunning(known)
	assert.Equal(t, first, b.records[known].runningSince, "the healthy run starts when the unit first becomes Running")
}

func TestNilRecoveryBackoffIsDisabled(t *testing.T) {
	var b *recoveryBackoff
	key := recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0"}

	wait, failures, started := b.start(key, "rev-1", 3*time.Second, false)
	assert.Equal(t, 3*time.Second, wait)
	assert.Zero(t, failures)
	assert.False(t, started)
	assert.False(t, b.finish(key))
	assert.Empty(t, b.pending("uid"))
	b.recordFailure(key)
	b.markRunning(key)
	b.forget(func(recoveryUnitKey) bool { return true })
}

func TestRecoveryUnitKeyFor(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "ms-0-prefill-0-0",
		Labels: map[string]string{
			workloadv1alpha1.RoleLabelKey: "prefill",
			workloadv1alpha1.RoleIDKey:    "prefill-0",
		},
	}}
	tests := []struct {
		name   string
		policy workloadv1alpha1.RecoveryPolicy
		want   recoveryUnitKey
		text   string
	}{
		{
			name:   "ServingGroupRecreate recovers the ServingGroup",
			policy: workloadv1alpha1.ServingGroupRecreate,
			want:   recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0"},
			text:   "ServingGroup ms-0",
		},
		{
			name:   "RoleRecreate recovers the Role instance",
			policy: workloadv1alpha1.RoleRecreate,
			want:   recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0", roleName: "prefill", roleID: "prefill-0"},
			text:   "Role prefill/prefill-0 in ServingGroup ms-0",
		},
		{
			name: "empty policy defaults to RoleRecreate",
			want: recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0", roleName: "prefill", roleID: "prefill-0"},
			text: "Role prefill/prefill-0 in ServingGroup ms-0",
		},
		{
			name:   "None recovers the Pod",
			policy: workloadv1alpha1.NoneRestartPolicy,
			want:   recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0", roleName: "prefill", roleID: "prefill-0", podName: "ms-0-prefill-0-0"},
			text:   "Pod ms-0-prefill-0-0 in ServingGroup ms-0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{UID: "uid"},
				Spec:       workloadv1alpha1.ModelServingSpec{RecoveryPolicy: tt.policy},
			}
			key := recoveryUnitKeyFor(ms, "ms-0", pod)
			assert.Equal(t, tt.want, key)
			assert.Equal(t, tt.text, key.String())
		})
	}
}

func TestForgetRecoveryUnits(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{UID: "uid"}}
	role0 := recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0", roleName: "prefill", roleID: "prefill-0"}
	role1 := recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0", roleName: "prefill", roleID: "prefill-1"}
	otherGroup := recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-1", roleName: "prefill", roleID: "prefill-0"}
	otherModelServing := recoveryUnitKey{modelServingUID: "other", servingGroup: "ms-0"}
	newController := func() *ModelServingController {
		b := newRecoveryBackoff(DefaultRecoveryBackoffConfig(), clocktesting.NewFakeClock(time.Now()))
		for _, key := range []recoveryUnitKey{role0, role1, otherGroup, otherModelServing} {
			b.records[key] = &recoveryRecord{failures: 1}
		}
		return &ModelServingController{recovery: b}
	}

	c := newController()
	c.forgetRecoveryUnits(ms, "ms-0", "prefill", "prefill-0")
	assert.NotContains(t, c.recovery.records, role0)
	assert.Len(t, c.recovery.records, 3)

	c = newController()
	c.forgetRecoveryUnits(ms, "ms-0", "", "")
	assert.NotContains(t, c.recovery.records, role0)
	assert.NotContains(t, c.recovery.records, role1)
	assert.Contains(t, c.recovery.records, otherGroup)
	assert.Contains(t, c.recovery.records, otherModelServing)
}

func TestSetRecoveryBackOffCondition(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC))
	c := &ModelServingController{recovery: newRecoveryBackoff(DefaultRecoveryBackoffConfig(), clk)}
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{UID: "uid", Generation: 2}}
	conditionType := string(workloadv1alpha1.ModelServingRecoveryBackOff)

	assert.False(t, c.setRecoveryBackOffCondition(ms))
	assert.Nil(t, apiMeta.FindStatusCondition(ms.Status.Conditions, conditionType), "a ModelServing that never backed off has no condition")

	key := recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-0", roleName: "prefill", roleID: "prefill-0"}
	c.recovery.records[key] = &recoveryRecord{failures: 2, identity: "rev-1/hash"}
	_, _, started := c.recovery.start(key, "rev-1/hash", 0, false)
	require.True(t, started)

	assert.True(t, c.setRecoveryBackOffCondition(ms))
	condition := apiMeta.FindStatusCondition(ms.Status.Conditions, conditionType)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, recoveryBackOffReason, condition.Reason)
	assert.Equal(t, "Role prefill/prefill-0 in ServingGroup ms-0 failed 3 times in a row, next recovery at 2026-11-03T08:00:20Z", condition.Message)
	assert.Equal(t, int64(2), condition.ObservedGeneration)
	assert.False(t, c.setRecoveryBackOffCondition(ms), "an unchanged backoff does not update the status")

	clk.Step(20 * time.Second)
	require.True(t, c.recovery.finish(key))
	assert.True(t, c.setRecoveryBackOffCondition(ms))
	condition = apiMeta.FindStatusCondition(ms.Status.Conditions, conditionType)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, noRecoveryBackOffReason, condition.Reason)
}

func TestSetRecoveryBackOffConditionTruncatesMessage(t *testing.T) {
	c := &ModelServingController{recovery: newRecoveryBackoff(DefaultRecoveryBackoffConfig(), clocktesting.NewFakeClock(time.Now()))}
	for i := 0; i < 20; i++ {
		key := recoveryUnitKey{modelServingUID: "uid", servingGroup: "ms-" + strings.Repeat("x", i)}
		c.recovery.records[key] = &recoveryRecord{failures: 1, identity: "rev-1"}
		_, _, started := c.recovery.start(key, "rev-1", 0, false)
		require.True(t, started)
	}
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{UID: "uid"}}

	require.True(t, c.setRecoveryBackOffCondition(ms))
	condition := apiMeta.FindStatusCondition(ms.Status.Conditions, string(workloadv1alpha1.ModelServingRecoveryBackOff))
	require.NotNil(t, condition)
	assert.Len(t, []rune(condition.Message), maxRecoveryBackOffMessageLength)
	assert.True(t, strings.HasSuffix(condition.Message, "..."))
}

const (
	recoveryTestNamespace = "default"
	recoveryTestName      = "backoff"
	recoveryTestGroup     = "backoff-0"
	recoveryTestRole      = "prefill"
	recoveryTestRevision  = "rev-1"
	recoveryTestHash      = "hash-1"
)

type recoveryFixture struct {
	controller *ModelServingController
	kubeClient *kubefake.Clientset
	clock      *clocktesting.FakeClock
	recorder   *record.FakeRecorder
	ms         *workloadv1alpha1.ModelServing
	msKey      types.NamespacedName
}

func newRecoveryFixture(t *testing.T, policy workloadv1alpha1.RecoveryPolicy, roleIDs ...string) *recoveryFixture {
	t.Helper()
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: recoveryTestNamespace, Name: recoveryTestName, UID: "ms-uid"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:       ptr.To[int32](1),
			RecoveryPolicy: policy,
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{{Name: recoveryTestRole, Replicas: ptr.To(int32(len(roleIDs)))}},
			},
		},
	}
	kubeClient := kubefake.NewSimpleClientset()
	c, err := NewModelServingController(kubeClient, kthenafake.NewSimpleClientset(ms.DeepCopy()), volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	t.Cleanup(c.workqueue.ShutDown)
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC))
	c.recovery = newRecoveryBackoff(DefaultRecoveryBackoffConfig(), clk)
	recorder := record.NewFakeRecorder(100)
	c.recorder = recorder
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))

	f := &recoveryFixture{
		controller: c,
		kubeClient: kubeClient,
		clock:      clk,
		recorder:   recorder,
		ms:         ms,
		msKey:      types.NamespacedName{Namespace: recoveryTestNamespace, Name: recoveryTestName},
	}
	for _, roleID := range roleIDs {
		c.store.AddServingGroupAndRole(f.msKey, recoveryTestGroup, recoveryTestRevision, recoveryTestHash, recoveryTestRole, roleID)
	}
	return f
}

// addFailedPod creates a failed Pod in the API and in the informer cache.
func (f *recoveryFixture) addFailedPod(t *testing.T, uid, roleID string, conditions ...corev1.PodCondition) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: recoveryTestNamespace,
			Name:      recoveryTestGroup + "-" + roleID + "-0",
			UID:       types.UID(uid),
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: recoveryTestName,
				workloadv1alpha1.GroupNameLabelKey:        recoveryTestGroup,
				workloadv1alpha1.RoleLabelKey:             recoveryTestRole,
				workloadv1alpha1.RoleIDKey:                roleID,
				workloadv1alpha1.RevisionLabelKey:         recoveryTestRevision,
				workloadv1alpha1.RoleTemplateHashLabelKey: recoveryTestHash,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
				Kind:       "ModelServing",
				Name:       recoveryTestName,
				UID:        f.ms.UID,
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, Conditions: conditions},
	}
	_, err := f.kubeClient.CoreV1().Pods(recoveryTestNamespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, f.controller.podsInformer.GetIndexer().Add(pod.DeepCopy()))
	return pod
}

func (f *recoveryFixture) deleted(uid string) bool {
	for _, action := range f.kubeClient.Actions() {
		deleteAction, ok := action.(kubetesting.DeleteAction)
		if !ok || !action.Matches("delete", "pods") {
			continue
		}
		if p := deleteAction.GetDeleteOptions().Preconditions; p != nil && p.UID != nil && string(*p.UID) == uid {
			return true
		}
	}
	return false
}

func (f *recoveryFixture) waitForDeletion(t *testing.T, uid string) {
	t.Helper()
	require.Eventually(t, func() bool { return f.deleted(uid) }, 5*time.Second, 10*time.Millisecond, "pod %s was not deleted", uid)
}

// observeDeletion delivers the informer delete event of a Pod the controller deleted.
func (f *recoveryFixture) observeDeletion(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	require.NoError(t, f.controller.podsInformer.GetIndexer().Delete(pod))
	f.controller.deletePod(pod)
}

// drainEvents returns the events recorded so far.
func (f *recoveryFixture) drainEvents() []string {
	var events []string
	for {
		select {
		case event := <-f.recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

func (f *recoveryFixture) failures(key recoveryUnitKey) int32 {
	f.controller.recovery.mu.Lock()
	defer f.controller.recovery.mu.Unlock()
	if rec := f.controller.recovery.records[key]; rec != nil {
		return rec.failures
	}
	return 0
}

func TestFailingRoleRecoveryBacksOff(t *testing.T) {
	f := newRecoveryFixture(t, workloadv1alpha1.RoleRecreate, "prefill-0")
	key := recoveryUnitKey{modelServingUID: f.ms.UID, servingGroup: recoveryTestGroup, roleName: recoveryTestRole, roleID: "prefill-0"}

	for round, wantWait := range []time.Duration{0, 10 * time.Second, 20 * time.Second, 40 * time.Second} {
		uid := "pod-" + string(rune('a'+round))
		pod := f.addFailedPod(t, uid, "prefill-0")
		require.NoError(t, f.controller.handleErrorPod(f.ms, recoveryTestGroup, pod))

		if wantWait > 0 {
			require.Eventually(t, f.clock.HasWaiters, 5*time.Second, 10*time.Millisecond)
			assert.Len(t, f.controller.recovery.pending(f.ms.UID), 1)
			assert.Contains(t, f.drainEvents(), fmt.Sprintf(
				"Warning RecoveryBackOff Role prefill/prefill-0 in ServingGroup backoff-0 has failed %d times in a row, delaying its recovery by %s",
				round+1, wantWait))

			f.clock.Step(wantWait - time.Second)
			assert.True(t, f.clock.HasWaiters(), "round %d: recovery started before the backoff expired", round)
			assert.False(t, f.deleted(uid), "round %d: pod deleted before the backoff expired", round)
			f.clock.Step(time.Second)
		}
		f.waitForDeletion(t, uid)
		assert.Empty(t, f.controller.recovery.pending(f.ms.UID))

		f.observeDeletion(t, pod)
		assert.Equal(t, int32(round+1), f.failures(key))
		assert.Contains(t, f.drainEvents(), "Normal RoleDeleting Role prefill/prefill-0 in ServingGroup backoff-0 is now Deleting",
			"round %d: RoleRecreate did not recreate the Role", round)

		// The controller recreates the Role with the same name.
		f.controller.store.DeleteRole(f.msKey, recoveryTestGroup, recoveryTestRole, "prefill-0")
		f.controller.store.AddRole(f.msKey, recoveryTestGroup, recoveryTestRole, "prefill-0", recoveryTestRevision, recoveryTestHash)
	}
}

func TestServingGroupRecoveryCountsOnePerRound(t *testing.T) {
	f := newRecoveryFixture(t, workloadv1alpha1.ServingGroupRecreate, "prefill-0", "prefill-1")
	key := recoveryUnitKey{modelServingUID: f.ms.UID, servingGroup: recoveryTestGroup}
	first := f.addFailedPod(t, "pod-a", "prefill-0")
	second := f.addFailedPod(t, "pod-b", "prefill-1")

	require.NoError(t, f.controller.handleErrorPod(f.ms, recoveryTestGroup, first))
	require.NoError(t, f.controller.handleErrorPod(f.ms, recoveryTestGroup, second))
	f.waitForDeletion(t, "pod-a")
	f.waitForDeletion(t, "pod-b")

	f.observeDeletion(t, first)
	f.observeDeletion(t, second)
	assert.Equal(t, int32(1), f.failures(key), "both Pods failed in the same round")
}

func TestDisruptedPodDoesNotCountTowardBackoff(t *testing.T) {
	tests := []struct {
		name         string
		conditions   []corev1.PodCondition
		wantFailures int32
	}{
		{name: "workload failure counts", wantFailures: 1},
		{
			name:       "eviction does not count",
			conditions: []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryFixture(t, workloadv1alpha1.NoneRestartPolicy, "prefill-0")
			pod := f.addFailedPod(t, "pod-a", "prefill-0", tt.conditions...)
			key := recoveryUnitKeyFor(f.ms, recoveryTestGroup, pod)

			require.NoError(t, f.controller.handleErrorPod(f.ms, recoveryTestGroup, pod))
			f.waitForDeletion(t, "pod-a")
			f.observeDeletion(t, pod)

			assert.Equal(t, tt.wantFailures, f.failures(key))
		})
	}
}

func TestPodDeletedByOthersDoesNotCountTowardBackoff(t *testing.T) {
	f := newRecoveryFixture(t, workloadv1alpha1.NoneRestartPolicy, "prefill-0")
	pod := f.addFailedPod(t, "pod-a", "prefill-0")
	key := recoveryUnitKeyFor(f.ms, recoveryTestGroup, pod)
	f.controller.recovery.records[key] = &recoveryRecord{identity: recoveryIdentity(key, pod)}

	f.observeDeletion(t, pod)

	assert.Zero(t, f.failures(key))
}

func TestUpdateModelServingStatusReportsRecoveryBackOff(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	controller, err := NewModelServingController(kubeClient, kthenaClient, nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC))
	controller.recovery = newRecoveryBackoff(DefaultRecoveryBackoffConfig(), clk)

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "status-backoff", UID: "ms-uid"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](1),
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
				Name: "decode", Replicas: ptr.To[int32](1),
			}}},
		},
	}
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Create(context.Background(), ms, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, controller.modelServingsInformer.GetIndexer().Add(ms))
	controller.store.AddServingGroup(types.NamespacedName{Namespace: ms.Namespace, Name: ms.Name}, 0, "revision")

	unit := recoveryUnitKey{modelServingUID: ms.UID, servingGroup: "status-backoff-0", roleName: "decode", roleID: "decode-0"}
	controller.recovery.records[unit] = &recoveryRecord{failures: 1, identity: "revision/hash"}
	_, _, started := controller.recovery.start(unit, "revision/hash", 0, false)
	require.True(t, started)

	getCondition := func() *metav1.Condition {
		t.Helper()
		updated, err := kthenaClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
		require.NoError(t, err)
		// Feed the written status back to the lister, as the informer would.
		require.NoError(t, controller.modelServingsInformer.GetIndexer().Update(updated))
		return apiMeta.FindStatusCondition(updated.Status.Conditions, string(workloadv1alpha1.ModelServingRecoveryBackOff))
	}

	require.NoError(t, controller.UpdateModelServingStatus(ms, "revision"))
	condition := getCondition()
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Contains(t, condition.Message, "Role decode/decode-0 in ServingGroup status-backoff-0 failed 2 times in a row")

	clk.Step(10 * time.Second)
	require.True(t, controller.recovery.finish(unit))
	require.NoError(t, controller.UpdateModelServingStatus(ms, "revision"))
	condition = getCondition()
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
}
