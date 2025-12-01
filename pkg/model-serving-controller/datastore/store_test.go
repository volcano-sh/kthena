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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"

	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func TestDeleteModelServing(t *testing.T) {
	key1 := types.NamespacedName{Namespace: "ns1", Name: "model1"}
	key2 := types.NamespacedName{Namespace: "ns2", Name: "model2"}

	s := &store{
		mutex: sync.RWMutex{},
		servingGroup: map[types.NamespacedName]map[string]*ServingGroup{
			key1: {},
			key2: {},
		},
	}

	// 1.Delete exist key
	s.DeleteModelServing(key1)
	_, exists := s.servingGroup[key1]
	assert.False(t, exists, "key1 should be deleted")

	// 2. Delete not exist key
	nonExistKey := types.NamespacedName{Namespace: "ns3", Name: "model3"}
	s.DeleteModelServing(nonExistKey)
	// Delete another key
	_, exists = s.servingGroup[key2]
	assert.True(t, exists, "key2 should still exist")

	// 3. Delete same key twice
	s.DeleteModelServing(key2)
	s.DeleteModelServing(key2)
	_, exists = s.servingGroup[key2]
	assert.False(t, exists, "key2 should be deleted after repeated deletes")
}

func TestDeleteServingGroup(t *testing.T) {
	key1 := types.NamespacedName{Namespace: "ns1", Name: "model1"}
	key2 := types.NamespacedName{Namespace: "ns2", Name: "model2"}

	s := &store{
		mutex: sync.RWMutex{},
		servingGroup: map[types.NamespacedName]map[string]*ServingGroup{
			key1: {
				"groupA": &ServingGroup{},
				"groupB": &ServingGroup{},
			},
			key2: {
				"groupC": &ServingGroup{},
			},
		},
	}

	// 1. Delete exist ServingGroupName
	s.DeleteServingGroup(key1, "groupA")
	_, exists := s.servingGroup[key1]["groupA"]
	assert.False(t, exists, "groupA should be deleted from key1")

	// 2. Delete not exist ServingGroupName
	s.DeleteServingGroup(key1, "groupX")
	_, exists = s.servingGroup[key1]["groupB"]
	assert.True(t, exists, "groupB should still exist in key1")

	// 3. modelServingName not exist
	nonExistKey := types.NamespacedName{Namespace: "ns3", Name: "model3"}
	s.DeleteServingGroup(nonExistKey, "groupY")

	// 4. Delete same ServingGroupName twice
	s.DeleteServingGroup(key2, "groupC")
	s.DeleteServingGroup(key2, "groupC")
	_, exists = s.servingGroup[key2]["groupC"]
	assert.False(t, exists, "groupC should be deleted from key2 after repeated deletes")
}

func TestAddServingGroup(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns1", Name: "model1"}

	s := &store{
		mutex:        sync.RWMutex{},
		servingGroup: make(map[types.NamespacedName]map[string]*ServingGroup),
	}

	// 1. Add group to an empty modelServing.
	s.AddServingGroup(key, 0, "9559767")
	groupName := utils.GenerateServingGroupName(key.Name, 0)
	group, exists := s.servingGroup[key][groupName]
	assert.True(t, exists, "group should exist after add")
	assert.Equal(t, groupName, group.Name)
	assert.Equal(t, ServingGroupCreating, group.Status)
	assert.NotNil(t, group.runningPods)
	assert.Empty(t, group.runningPods)

	// 2. Adds a group to an existing modelServingName.
	s.AddServingGroup(key, 1, "9559767")
	groupName2 := utils.GenerateServingGroupName(key.Name, 1)
	group2, exists2 := s.servingGroup[key][groupName2]
	assert.True(t, exists2, "second group should exist after add")
	assert.Equal(t, groupName2, group2.Name)

	// 3. Multiple additions of the same idx, group is overwritten
	s.AddServingGroup(key, 1, "9559767")
	group3, exists3 := s.servingGroup[key][groupName2]
	assert.True(t, exists3, "group should still exist after overwrite")
	assert.Equal(t, groupName2, group3.Name)

	// 4. new modelServingName
	key2 := types.NamespacedName{Namespace: "ns2", Name: "model2"}
	s.AddServingGroup(key2, 0, "9559766")
	groupName4 := utils.GenerateServingGroupName(key2.Name, 0)
	group4, exists4 := s.servingGroup[key2][groupName4]
	assert.True(t, exists4, "group for new modelServingName should exist")
	assert.Equal(t, groupName4, group4.Name)
}

func TestGetRoleList(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns1", Name: "model1"}

	s := &store{
		mutex: sync.RWMutex{},
		servingGroup: map[types.NamespacedName]map[string]*ServingGroup{
			key: {
				"group0": &ServingGroup{
					Name: "group0",
					roles: map[string]map[string]*Role{
						"prefill": {
							"prefill-0": &Role{Name: "prefill-0", Status: RoleCreating},
							"prefill-1": &Role{Name: "prefill-1", Status: RoleCreating},
						},
						"decode": {
							"decode-0": &Role{Name: "decode-0", Status: RoleCreating},
						},
					},
				},
			},
		},
	}

	// 1. Get existing roles
	roles, err := s.GetRoleList(key, "group0", "prefill")
	assert.NoError(t, err)
	assert.Len(t, roles, 2)
	// Check if sorted by name
	assert.Equal(t, "prefill-0", roles[0].Name)
	assert.Equal(t, "prefill-1", roles[1].Name)

	// 2. Get roles with non-existing roleLabel (should return empty list)
	roles, err = s.GetRoleList(key, "group0", "nonexist")
	assert.NoError(t, err)
	assert.Empty(t, roles)

	// 3. Get roles with non-existing modelServing
	nonExistKey := types.NamespacedName{Namespace: "ns2", Name: "model2"}
	roles, err = s.GetRoleList(nonExistKey, "group0", "prefill")
	assert.Error(t, err)
	assert.Nil(t, roles)

	// 4. Get roles with non-existing group
	roles, err = s.GetRoleList(key, "nonexistgroup", "prefill")
	assert.Error(t, err)
	assert.Equal(t, ErrServingGroupNotFound, err)
	assert.Nil(t, roles)
}

func TestUpdateRoleStatus(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns1", Name: "model1"}

	s := &store{
		mutex: sync.RWMutex{},
		servingGroup: map[types.NamespacedName]map[string]*ServingGroup{
			key: {
				"group0": &ServingGroup{
					Name: "group0",
					roles: map[string]map[string]*Role{
						"prefill": {
							"prefill-0": &Role{Name: "prefill-0", Status: RoleCreating},
						},
					},
				},
			},
		},
	}

	// 1. Update existing role status
	err := s.UpdateRoleStatus(key, "group0", "prefill", "prefill-0", RoleDeleting)
	assert.NoError(t, err)
	role := s.servingGroup[key]["group0"].roles["prefill"]["prefill-0"]
	assert.Equal(t, RoleDeleting, role.Status)

	// 2. Update non-existing modelServing
	nonExistKey := types.NamespacedName{Namespace: "ns2", Name: "model2"}
	err = s.UpdateRoleStatus(nonExistKey, "group0", "prefill", "prefill-0", RoleDeleting)
	assert.Error(t, err)

	// 3. Update non-existing group
	err = s.UpdateRoleStatus(key, "nonexistgroup", "prefill", "prefill-0", RoleDeleting)
	assert.Error(t, err)
	assert.Equal(t, ErrServingGroupNotFound, err)

	// 4. Update non-existing roleLabel
	err = s.UpdateRoleStatus(key, "group0", "nonexistrole", "prefill-0", RoleDeleting)
	assert.Error(t, err)

	// 5. Update non-existing roleName
	err = s.UpdateRoleStatus(key, "group0", "prefill", "nonexistname", RoleDeleting)
	assert.Error(t, err)
}

func TestDeleteRole(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns1", Name: "model1"}

	s := &store{
		mutex: sync.RWMutex{},
		servingGroup: map[types.NamespacedName]map[string]*ServingGroup{
			key: {
				"group0": &ServingGroup{
					Name: "group0",
					roles: map[string]map[string]*Role{
						"prefill": {
							"prefill-0": &Role{Name: "prefill-0"},
							"prefill-1": &Role{Name: "prefill-1"},
						},
					},
				},
			},
		},
	}

	// 1. Delete existing role
	s.DeleteRole(key, "group0", "prefill", "prefill-0")
	_, exists := s.servingGroup[key]["group0"].roles["prefill"]["prefill-0"]
	assert.False(t, exists, "role should be deleted")
	_, exists = s.servingGroup[key]["group0"].roles["prefill"]["prefill-1"]
	assert.True(t, exists, "other role should still exist")

	// 2. Delete non-existing role (should not panic)
	s.DeleteRole(key, "group0", "prefill", "nonexistrole")

	// 3. Delete role with non-existing roleLabel (should not panic)
	s.DeleteRole(key, "group0", "nonexistlabel", "prefill-1")

	// 4. Delete role with non-existing group (should not panic)
	s.DeleteRole(key, "nonexistgroup", "prefill", "prefill-1")

	// 5. Delete role with non-existing modelServing (should not panic)
	nonExistKey := types.NamespacedName{Namespace: "ns2", Name: "model2"}
	s.DeleteRole(nonExistKey, "group0", "prefill", "prefill-1")
}

func TestAddRole(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns1", Name: "model1"}

	s := &store{
		mutex:        sync.RWMutex{},
		servingGroup: make(map[types.NamespacedName]map[string]*ServingGroup),
	}

	// 1. Add role to non-existing modelServing and group
	s.AddRole(key, "group0", "prefill", "prefill-0", "revision1")
	role, exists := s.servingGroup[key]["group0"].roles["prefill"]["prefill-0"]
	assert.True(t, exists, "role should be created")
	assert.Equal(t, "prefill-0", role.Name)
	assert.Equal(t, RoleCreating, role.Status)
	assert.Equal(t, "revision1", role.Revision)

	// 2. Add another role to existing group
	s.AddRole(key, "group0", "prefill", "prefill-1", "revision2")
	role2, exists2 := s.servingGroup[key]["group0"].roles["prefill"]["prefill-1"]
	assert.True(t, exists2, "second role should be created")
	assert.Equal(t, "prefill-1", role2.Name)

	// 3. Add role with different roleLabel
	s.AddRole(key, "group0", "decode", "decode-0", "revision3")
	_, exists3 := s.servingGroup[key]["group0"].roles["decode"]["decode-0"]
	assert.True(t, exists3, "role with different label should be created")

	// 4. Overwrite existing role
	s.AddRole(key, "group0", "prefill", "prefill-0", "revision4")
	role4 := s.servingGroup[key]["group0"].roles["prefill"]["prefill-0"]
	assert.Equal(t, "revision4", role4.Revision, "role should be overwritten")
}

func TestGetServingGroupByModelServingSortingByIndex(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns1", Name: "model1"}

	s := &store{
		mutex:        sync.RWMutex{},
		servingGroup: make(map[types.NamespacedName]map[string]*ServingGroup),
	}

	// Add groups with non-sequential indices to test sorting by index
	// Add in arbitrary order to ensure sorting is by index, not insertion order
	s.AddServingGroup(key, 10, "revision")
	s.AddServingGroup(key, 2, "revision")
	s.AddServingGroup(key, 5, "revision")
	s.AddServingGroup(key, 1, "revision")

	groups, err := s.GetServingGroupByModelServing(key)
	assert.NoError(t, err)
	assert.Len(t, groups, 4)

	// Verify groups are sorted by index in ascending order
	// With string sorting, model1-10 would come before model1-2
	// With index sorting, order should be: model1-1, model1-2, model1-5, model1-10
	expectedNames := []string{
		utils.GenerateServingGroupName(key.Name, 1),
		utils.GenerateServingGroupName(key.Name, 2),
		utils.GenerateServingGroupName(key.Name, 5),
		utils.GenerateServingGroupName(key.Name, 10),
	}

	for i, group := range groups {
		assert.Equal(t, expectedNames[i], group.Name, "Groups should be sorted by index, not by name")
	}
}

func TestGetRoleListSortingByIndex(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns1", Name: "model1"}

	s := &store{
		mutex: sync.RWMutex{},
		servingGroup: map[types.NamespacedName]map[string]*ServingGroup{
			key: {
				"group0": &ServingGroup{
					Name: "group0",
					roles: map[string]map[string]*Role{
						"prefill": {
							"prefill-10": &Role{Name: "prefill-10", Status: RoleCreating},
							"prefill-2":  &Role{Name: "prefill-2", Status: RoleCreating},
							"prefill-5":  &Role{Name: "prefill-5", Status: RoleCreating},
							"prefill-1":  &Role{Name: "prefill-1", Status: RoleCreating},
						},
					},
				},
			},
		},
	}

	roles, err := s.GetRoleList(key, "group0", "prefill")
	assert.NoError(t, err)
	assert.Len(t, roles, 4)

	// Verify roles are sorted by index in ascending order
	// With string sorting, prefill-10 would come before prefill-2
	// With index sorting, order should be: prefill-1, prefill-2, prefill-5, prefill-10
	expectedNames := []string{
		"prefill-1",
		"prefill-2",
		"prefill-5",
		"prefill-10",
	}

	for i, role := range roles {
		assert.Equal(t, expectedNames[i], role.Name, "Roles should be sorted by index, not by name")
	}
}
