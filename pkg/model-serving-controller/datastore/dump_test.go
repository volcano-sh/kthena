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

package datastore

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestStore_DumpCache(t *testing.T) {
	s := New()

	msName := types.NamespacedName{Namespace: "default", Name: "test-ms"}
	groupName := "test-ms-0"
	revision := "rev-1"
	roleName := "worker"
	roleID := "worker-0"
	roleTemplateHash := "hash-1"
	podName := "pod-1"

	s.AddServingGroup(msName, 0, revision)
	s.AddRole(msName, groupName, roleName, roleID, revision, roleTemplateHash)
	s.AddRunningPodToServingGroup(msName, groupName, podName, revision, roleTemplateHash, roleName, roleID)

	data, err := s.DumpCache()
	if err != nil {
		t.Fatalf("DumpCache returned error: %v", err)
	}
	if data == nil {
		t.Fatalf("DumpCache returned nil")
	}

	var cache map[string]map[string]ExportedServingGroup
	err = json.Unmarshal(data, &cache)
	if err != nil {
		t.Fatalf("Failed to unmarshal cache data: %v", err)
	}

	msKey := msName.String()
	groups, ok := cache[msKey]
	if !ok {
		t.Fatalf("Expected model serving %s in cache", msKey)
	}

	group, ok := groups[groupName]
	if !ok {
		t.Fatalf("Expected serving group %s in cache", groupName)
	}

	if group.Name != groupName {
		t.Errorf("Expected group name %s, got %s", groupName, group.Name)
	}
	if group.Revision != revision {
		t.Errorf("Expected group revision %s, got %s", revision, group.Revision)
	}

	foundPod := false
	for _, p := range group.RunningPods {
		if p == podName {
			foundPod = true
			break
		}
	}
	if !foundPod {
		t.Errorf("Expected running pod %s in group cache", podName)
	}

	rolesMap, ok := group.Roles[roleName]
	if !ok {
		t.Fatalf("Expected role %s in group roles", roleName)
	}

	role, ok := rolesMap[roleID]
	if !ok {
		t.Fatalf("Expected role ID %s in map", roleID)
	}

	if role.Name != roleID {
		t.Errorf("Expected role name %s, got %s", roleID, role.Name)
	}
	if role.Revision != revision {
		t.Errorf("Expected role revision %s, got %s", revision, role.Revision)
	}
	if role.RoleTemplateHash != roleTemplateHash {
		t.Errorf("Expected role hash %s, got %s", roleTemplateHash, role.RoleTemplateHash)
	}
}
