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

package v1alpha1

const (
	// ModelServingNameLabelKey is the pod label key for the model serving name.
	ModelServingNameLabelKey = "modelserving.volcano.sh/name"
	// GroupNameLabelKey is the pod label key for the group name.
	GroupNameLabelKey = "modelserving.volcano.sh/group-name"
	// RoleLabelKey is the pod label key for the role.
	RoleLabelKey = "modelserving.volcano.sh/role"
	// RoleIDKey is the pod label key for the role serial number.
	RoleIDKey = "modelserving.volcano.sh/role-id"
	// EntryLabelKey is the entry pod label key.
	EntryLabelKey = "modelserving.volcano.sh/entry"

	// RevisionLabelKey is the revision label for the model serving.
	RevisionLabelKey = "modelserving.volcano.sh/revision"
	// RoleTemplateHashLabelKey is the revision label for the role, used for RoleRollingUpdate strategy.
	RoleTemplateHashLabelKey = "modelserving.volcano.sh/role-template-hash"

	// TrafficDrainingAnnotation marks a pod as draining (lossless upgrade): the
	// model-serving-controller sets it to an RFC3339 timestamp when it starts
	// draining, signalling the router to stop scheduling new requests to the pod.
	TrafficDrainingAnnotation = "modelserving.volcano.sh/traffic-draining"
	// TrafficDrainedAnnotation marks a pod as drained (lossless upgrade): the
	// router sets it to "true" once the pod's in-flight requests reach zero,
	// signalling the controller it is safe to delete the pod.
	TrafficDrainedAnnotation = "modelserving.volcano.sh/traffic-drained"
)
