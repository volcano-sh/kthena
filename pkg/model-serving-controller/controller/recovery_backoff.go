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
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	// DefaultRecoveryBackoffBase is the delay before the second consecutive recovery of a unit.
	DefaultRecoveryBackoffBase = 10 * time.Second
	// DefaultRecoveryBackoffMax caps the delay between consecutive recoveries of a unit.
	DefaultRecoveryBackoffMax = 5 * time.Minute
	// DefaultRecoveryBackoffResetAfter is how long a unit must stay Running before its failure count resets.
	DefaultRecoveryBackoffResetAfter = 10 * time.Minute

	// maxCountedFailures bounds the failure counter.
	// The delay reaches its cap long before.
	maxCountedFailures = 64
	// maxRecoveryBackOffMessageLength bounds the condition message, independent of the number of units.
	maxRecoveryBackOffMessageLength = 512

	recoveryBackOffReason   = "RecoveryBackOff"
	noRecoveryBackOffReason = "NoRecoveryBackOff"
)

// RecoveryBackoffConfig configures the delay between consecutive recoveries of a failing unit.
type RecoveryBackoffConfig struct {
	// Base is the delay before the second consecutive recovery.
	// Zero disables the backoff.
	Base time.Duration
	// Max caps the delay.
	Max time.Duration
	// ResetAfter is how long a unit must stay Running before its failure count resets.
	ResetAfter time.Duration
}

// DefaultRecoveryBackoffConfig returns the backoff used when no configuration is given.
func DefaultRecoveryBackoffConfig() RecoveryBackoffConfig {
	return RecoveryBackoffConfig{
		Base:       DefaultRecoveryBackoffBase,
		Max:        DefaultRecoveryBackoffMax,
		ResetAfter: DefaultRecoveryBackoffResetAfter,
	}
}

// Validate checks that the configuration describes a usable backoff.
func (c RecoveryBackoffConfig) Validate() error {
	if c.Base < 0 || c.Max < 0 || c.ResetAfter < 0 {
		return fmt.Errorf("recovery backoff durations must not be negative: base=%s, max=%s, reset=%s", c.Base, c.Max, c.ResetAfter)
	}
	if c.Base > 0 && c.Max < c.Base {
		return fmt.Errorf("recovery backoff max (%s) must not be less than base (%s)", c.Max, c.Base)
	}
	return nil
}

// delay returns the backoff after the given number of consecutive recoveries.
// The first recovery is immediate, and each later one doubles the delay up to Max.
func (c RecoveryBackoffConfig) delay(failures int32) time.Duration {
	if c.Base <= 0 || failures <= 0 {
		return 0
	}
	d := c.Base
	for i := int32(1); i < failures && d < c.Max; i++ {
		d *= 2
	}
	return min(d, c.Max)
}

// recoveryUnitKey identifies the unit that the recovery policy recreates:
// the ServingGroup for ServingGroupRecreate, the Role instance for RoleRecreate, and the Pod for None.
type recoveryUnitKey struct {
	modelServingUID types.UID
	servingGroup    string
	roleName        string
	roleID          string
	podName         string
}

func recoveryUnitKeyFor(ms *workloadv1alpha1.ModelServing, servingGroupName string, pod *corev1.Pod) recoveryUnitKey {
	key := recoveryUnitKey{modelServingUID: ms.UID, servingGroup: servingGroupName}
	switch ms.Spec.RecoveryPolicy {
	case workloadv1alpha1.ServingGroupRecreate:
	case workloadv1alpha1.NoneRestartPolicy:
		key.roleName, key.roleID, key.podName = utils.GetRoleName(pod), utils.GetRoleID(pod), pod.Name
	default:
		key.roleName, key.roleID = utils.GetRoleName(pod), utils.GetRoleID(pod)
	}
	return key
}

func (k recoveryUnitKey) String() string {
	switch {
	case k.podName != "":
		return fmt.Sprintf("Pod %s in ServingGroup %s", k.podName, k.servingGroup)
	case k.roleID != "":
		return fmt.Sprintf("Role %s/%s in ServingGroup %s", k.roleName, k.roleID, k.servingGroup)
	default:
		return fmt.Sprintf("ServingGroup %s", k.servingGroup)
	}
}

// recoveryIdentity is the workload version of the failed Pod.
// Pods of one ServingGroup can carry different Role template hashes, so a ServingGroup uses the revision only.
func recoveryIdentity(key recoveryUnitKey, pod *corev1.Pod) string {
	if key.roleID == "" {
		return utils.ObjectRevision(pod)
	}
	return utils.ObjectRevision(pod) + "/" + utils.ObjectRoleTemplateHash(pod)
}

type recoveryRecord struct {
	// failures is the number of consecutive recoveries of the unit.
	failures int32
	// identity is the workload version that failed.
	identity string
	// runningSince is when the unit last became Running, or zero.
	runningSince time.Time
	// recoverAt is when a recovery delayed by the backoff is due, or zero.
	recoverAt time.Time
}

type pendingRecovery struct {
	key       recoveryUnitKey
	failures  int32
	recoverAt time.Time
}

// recoveryBackoff tracks consecutive failures of recovery units in memory.
// A nil *recoveryBackoff disables the backoff.
type recoveryBackoff struct {
	config  RecoveryBackoffConfig
	clock   clock.Clock
	mu      sync.Mutex
	records map[recoveryUnitKey]*recoveryRecord
}

func newRecoveryBackoff(config RecoveryBackoffConfig, clk clock.Clock) *recoveryBackoff {
	return &recoveryBackoff{
		config:  config,
		clock:   clk,
		records: map[recoveryUnitKey]*recoveryRecord{},
	}
}

// start returns how long to wait before recovering a failed Pod of the unit.
// started is true when this call put the unit into backoff.
func (b *recoveryBackoff) start(key recoveryUnitKey, identity string, grace time.Duration, disrupted bool) (wait time.Duration, failures int32, started bool) {
	if b == nil {
		return grace, 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()
	rec := b.records[key]
	if rec == nil {
		rec = &recoveryRecord{identity: identity}
		b.records[key] = rec
	}
	// A new workload version or a long healthy run starts a new failure sequence.
	if rec.identity != identity || (!rec.runningSince.IsZero() && now.Sub(rec.runningSince) >= b.config.ResetAfter) {
		rec.failures = 0
		rec.identity = identity
	}
	rec.runningSince = time.Time{}
	// Disruptions are not caused by the workload and keep the plain grace period.
	if disrupted {
		return grace, rec.failures, false
	}

	// Pods of the same unit that fail later share the pending recovery time.
	if !rec.recoverAt.IsZero() {
		return max(grace, rec.recoverAt.Sub(now)), rec.failures, false
	}
	delay := b.config.delay(rec.failures)
	if delay <= grace {
		return grace, rec.failures, false
	}
	rec.recoverAt = now.Add(delay)
	return delay, rec.failures, true
}

// finish clears the pending recovery of the unit once it is due, and reports whether it cleared one.
// A timer left over from an earlier round of the unit does not clear a later pending recovery.
func (b *recoveryBackoff) finish(key recoveryUnitKey) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.records[key]
	if rec == nil || rec.recoverAt.IsZero() || rec.recoverAt.After(b.clock.Now()) {
		return false
	}
	rec.recoverAt = time.Time{}
	return true
}

// recordFailure counts one recovery of the unit.
func (b *recoveryBackoff) recordFailure(key recoveryUnitKey) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if rec := b.records[key]; rec != nil && rec.failures < maxCountedFailures {
		rec.failures++
	}
}

// markRunning records when a unit with a failure history became Running.
func (b *recoveryBackoff) markRunning(key recoveryUnitKey) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if rec := b.records[key]; rec != nil && rec.runningSince.IsZero() {
		rec.runningSince = b.clock.Now()
	}
}

// forget drops the records whose keys match.
func (b *recoveryBackoff) forget(match func(recoveryUnitKey) bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.records {
		if match(key) {
			delete(b.records, key)
		}
	}
}

// pending lists the units of a ModelServing whose recovery is delayed by the backoff.
func (b *recoveryBackoff) pending(modelServingUID types.UID) []pendingRecovery {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var result []pendingRecovery
	for key, rec := range b.records {
		if key.modelServingUID == modelServingUID && !rec.recoverAt.IsZero() {
			result = append(result, pendingRecovery{key: key, failures: rec.failures, recoverAt: rec.recoverAt})
		}
	}
	slices.SortFunc(result, func(a, b pendingRecovery) int {
		return strings.Compare(a.key.String(), b.key.String())
	})
	return result
}

// isDisrupted reports whether the Pod failed because of an eviction, a preemption, or a node taint.
func isDisrupted(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.DisruptionTarget && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func restartGracePeriod(ms *workloadv1alpha1.ModelServing) time.Duration {
	if ms.Spec.Template.RestartGracePeriodSeconds == nil || *ms.Spec.Template.RestartGracePeriodSeconds <= 0 {
		return 0
	}
	return time.Duration(*ms.Spec.Template.RestartGracePeriodSeconds) * time.Second
}

// SetRecoveryBackoff replaces the backoff between consecutive recoveries of a failing unit.
// It must be called before the controller runs.
func (c *ModelServingController) SetRecoveryBackoff(config RecoveryBackoffConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}
	clk := clock.Clock(clock.RealClock{})
	if c.recovery != nil {
		clk = c.recovery.clock
	}
	c.recovery = newRecoveryBackoff(config, clk)
	return nil
}

// recoveryWait returns how long to wait before recovering the failed Pod.
// It is the longer of the restart grace period and the backoff of the Pod's recovery unit.
func (c *ModelServingController) recoveryWait(ms *workloadv1alpha1.ModelServing, servingGroupName string, pod *corev1.Pod) time.Duration {
	grace := restartGracePeriod(ms)
	key := recoveryUnitKeyFor(ms, servingGroupName, pod)
	wait, failures, started := c.recovery.start(key, recoveryIdentity(key, pod), grace, isDisrupted(pod))
	if started {
		message := fmt.Sprintf("%s has failed %d times in a row, delaying its recovery by %s", key, failures+1, wait.Round(time.Second))
		c.emitRoleStatusEvent(ms, corev1.EventTypeWarning, recoveryBackOffReason, message)
	}
	return wait
}

// sleepForRecovery blocks for the given duration on the controller's clock.
func (c *ModelServingController) sleepForRecovery(d time.Duration) {
	if c.recovery != nil {
		<-c.recovery.clock.After(d)
		return
	}
	<-time.After(d)
}

// markRecoveryUnitRunning resets the healthy-run timer of the unit that the ready Pod belongs to.
func (c *ModelServingController) markRecoveryUnitRunning(ms *workloadv1alpha1.ModelServing, servingGroupName string, pod *corev1.Pod, roleReady, servingGroupReady bool) {
	key := recoveryUnitKeyFor(ms, servingGroupName, pod)
	switch {
	case key.podName != "":
	case key.roleID != "" && roleReady:
	case key.roleID == "" && servingGroupReady:
	default:
		return
	}
	c.recovery.markRunning(key)
}

// forgetRecoveryUnits drops the failure history of a ServingGroup, or of one Role instance when roleID is set.
func (c *ModelServingController) forgetRecoveryUnits(ms *workloadv1alpha1.ModelServing, servingGroupName, roleName, roleID string) {
	c.recovery.forget(func(key recoveryUnitKey) bool {
		if key.modelServingUID != ms.UID || key.servingGroup != servingGroupName {
			return false
		}
		return roleID == "" || (key.roleName == roleName && key.roleID == roleID)
	})
}

// setRecoveryBackOffCondition reports the units whose recovery is delayed by the backoff.
// The condition is only added once a unit has been in backoff, so healthy ModelServings keep their status unchanged.
func (c *ModelServingController) setRecoveryBackOffCondition(ms *workloadv1alpha1.ModelServing) bool {
	conditionType := string(workloadv1alpha1.ModelServingRecoveryBackOff)
	existing := apiMeta.FindStatusCondition(ms.Status.Conditions, conditionType)
	pending := c.recovery.pending(ms.UID)
	if len(pending) == 0 && existing == nil {
		return false
	}

	condition := metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionFalse,
		Reason:             noRecoveryBackOffReason,
		Message:            "No failed unit is waiting for recovery",
		ObservedGeneration: ms.Generation,
	}
	if len(pending) > 0 {
		parts := make([]string, 0, len(pending))
		for _, p := range pending {
			parts = append(parts, fmt.Sprintf("%s failed %d times in a row, next recovery at %s",
				p.key, p.failures+1, p.recoverAt.UTC().Format(time.RFC3339)))
		}
		condition.Status = metav1.ConditionTrue
		condition.Reason = recoveryBackOffReason
		condition.Message = strings.Join(parts, "; ")
		if text := []rune(condition.Message); len(text) > maxRecoveryBackOffMessageLength {
			condition.Message = string(text[:maxRecoveryBackOffMessageLength-3]) + "..."
		}
	}

	changed := existing == nil || existing.Status != condition.Status || existing.Reason != condition.Reason ||
		existing.Message != condition.Message || existing.ObservedGeneration != condition.ObservedGeneration
	apiMeta.SetStatusCondition(&ms.Status.Conditions, condition)
	return changed
}
