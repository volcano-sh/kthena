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

// ResourceType identifies the informer resource represented by a QueueItem.
type ResourceType string

const (
	ResourceTypeModelServer           ResourceType = "ModelServer"
	ResourceTypePod                   ResourceType = "Pod"
	ResourceTypeSecret                ResourceType = "Secret"
	ResourceTypeExternalModelProvider ResourceType = "ExternalModelProvider"
)

// QueueItem is the typed workqueue key shared by controllers that reconcile
// more than one Kubernetes resource type.
type QueueItem struct {
	ResourceType ResourceType
	Key          string
}
