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
	"errors"
	"fmt"
	"slices"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// Store is an interface for storing and retrieving data
type Store interface {
	GetServingGroupByModelServing(modelServingName types.NamespacedName) ([]ServingGroup, error)
	GetServingGroup(modelServingName types.NamespacedName, groupName string) *ServingGroup
	GetRunningPodNumByServingGroup(modelServingName types.NamespacedName, groupName string) (int, error)
	GetServingGroupStatus(modelServingName types.NamespacedName, groupName string) ServingGroupStatus
	GetRoleList(modelServingName types.NamespacedName, groupName, roleName string) ([]Role, error)
	GetRoleStatus(modelServingName types.NamespacedName, groupName, roleName, roleID string) RoleStatus
	UpdateRoleStatus(modelServingName types.NamespacedName, groupName, roleName, roleID string, status RoleStatus) error
	DeleteRole(modelServingName types.NamespacedName, groupName, roleName, roleID string)
	DeleteModelServing(modelServingName types.NamespacedName)
	DeleteServingGroup(modelServingName types.NamespacedName, groupName string)
	AddServingGroup(modelServingName types.NamespacedName, idx int, revision string)
	AddRole(modelServingName types.NamespacedName, groupName, roleName, roleID, revision string)
	AddRunningPodToServingGroup(modelServingName types.NamespacedName, groupName, pod, revision, roleName, roleID string)
	// AddServingGroupAndRole adds servingGroup and role if not exist
	AddServingGroupAndRole(modelServingName types.NamespacedName, servingGroupName, revision, roleName, roleID string)
	DeleteRunningPodFromServingGroup(modelServingName types.NamespacedName, groupName string, pod string)
	UpdateServingGroupStatus(modelServingName types.NamespacedName, groupName string, Status ServingGroupStatus) error
}

type store struct {
	mutex sync.RWMutex

	// ServingGroup is a map of modelServing names to their ServingGroups
	// modelServing -> group name-> ServingGroup
	servingGroup map[types.NamespacedName]map[string]*ServingGroup
}

type ServingGroup struct {
	Name        string
	runningPods map[string]struct{} // Map of pod names in this ServingGroup
	Revision    string
	Status      ServingGroupStatus
	roles       map[string]map[string]*Role // roleName -> roleID -> *Role, like prefill -> prefill-0 -> *Role
}

type Role struct {
	Name     string
	Revision string
	Status   RoleStatus
}

type ServingGroupStatus string

const (
	ServingGroupRunning  ServingGroupStatus = "Running"
	ServingGroupCreating ServingGroupStatus = "Creating"
	ServingGroupDeleting ServingGroupStatus = "Deleting"
	ServingGroupScaling  ServingGroupStatus = "Scaling"
	ServingGroupNotFound ServingGroupStatus = "NotFound"
)

type RoleStatus string

const (
	RoleCreating RoleStatus = "Creating"
	RoleRunning  RoleStatus = "Running"
	RoleDeleting RoleStatus = "Deleting"
	RoleNotFound RoleStatus = "NotFound"
)

var ErrServingGroupNotFound = errors.New("serving group not found")

func New() Store {
	return &store{
		servingGroup: make(map[types.NamespacedName]map[string]*ServingGroup),
	}
}

// GetServingGroupByModelServing returns the list of ServingGroups and errors
func (s *store) GetServingGroupByModelServing(modelServingName types.NamespacedName) ([]ServingGroup, error) {
	s.mutex.RLock()
	servingGroups, ok := s.servingGroup[modelServingName]
	if !ok {
		s.mutex.RUnlock()
		return nil, ErrServingGroupNotFound
	}
	// sort ServingGroups by index
	servingGroupsSlice := make([]ServingGroup, 0, len(servingGroups))
	for _, servingGroup := range servingGroups {
		// This is a clone to prevent r/w conflict later
		servingGroupsSlice = append(servingGroupsSlice, *servingGroup)
	}
	s.mutex.RUnlock()

	slices.SortFunc(servingGroupsSlice, func(a, b ServingGroup) int {
		_, aIndex := utils.GetParentNameAndOrdinal(a.Name)
		_, bIndex := utils.GetParentNameAndOrdinal(b.Name)
		return aIndex - bIndex
	})

	return servingGroupsSlice, nil
}

// GetRoleList returns the list of roles and errors
func (s *store) GetRoleList(modelServingName types.NamespacedName, groupName, roleName string) ([]Role, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	servingGroups, ok := s.servingGroup[modelServingName]
	if !ok {
		return nil, fmt.Errorf("cannot list ServingGroup of modelServing %s", modelServingName.Name)
	}
	servingGroup, ok := servingGroups[groupName]
	if !ok {
		return nil, ErrServingGroupNotFound
	}
	roleMap, ok := servingGroup.roles[roleName]
	if !ok {
		// If the roleName does not exist, return an empty list instead of an error
		return []Role{}, nil
	}

	//Convert roles in map to a slice
	roleSlice := make([]Role, 0, len(roleMap))
	for _, role := range roleMap {
		roleSlice = append(roleSlice, *role)
	}

	slices.SortFunc(roleSlice, func(a, b Role) int {
		_, aIndex := utils.GetParentNameAndOrdinal(a.Name)
		_, bIndex := utils.GetParentNameAndOrdinal(b.Name)
		return aIndex - bIndex
	})

	return roleSlice, nil
}

// UpdateRoleStatus updates the status of a specific role
func (s *store) UpdateRoleStatus(modelServingName types.NamespacedName, groupName, roleName, roleID string, status RoleStatus) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	servingGroups, ok := s.servingGroup[modelServingName]
	if !ok {
		return fmt.Errorf("cannot find modelServing %s", modelServingName.Name)
	}

	servingGroup, ok := servingGroups[groupName]
	if !ok {
		return ErrServingGroupNotFound
	}

	roleMap, ok := servingGroup.roles[roleName]
	if !ok {
		return fmt.Errorf("roleName %s not found in group %s", roleName, groupName)
	}

	role, ok := roleMap[roleID]
	if !ok {
		return fmt.Errorf("role %s not found in roleName %s of group %s", roleID, roleName, groupName)
	}

	role.Status = status
	return nil
}

// GetRoleStatus returns the status of a specific role
func (s *store) GetRoleStatus(modelServingName types.NamespacedName, groupName, roleName, roleID string) RoleStatus {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if servingGroups, exist := s.servingGroup[modelServingName]; exist {
		if group, ok := servingGroups[groupName]; ok {
			if roleMap, exists := group.roles[roleName]; exists {
				if role, found := roleMap[roleID]; found {
					return role.Status
				}
			}
		}
	}
	return RoleNotFound
}

// GetRunningPodNumByServingGroup returns the number of running pods and errors
func (s *store) GetRunningPodNumByServingGroup(modelServingName types.NamespacedName, groupName string) (int, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	groups, ok := s.servingGroup[modelServingName]
	if !ok {
		return 0, fmt.Errorf("modelServing %s not found", modelServingName)
	}

	group, ok := groups[groupName]
	if !ok {
		return 0, nil
	}
	return len(group.runningPods), nil
}

// GetServingGroup returns the GetServingGroup
func (s *store) GetServingGroup(modelServingName types.NamespacedName, groupName string) *ServingGroup {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	groups, ok := s.servingGroup[modelServingName]
	if !ok {
		return nil
	}

	return groups[groupName]
}

// GetServingGroupStatus returns the status of ServingGroup
func (s *store) GetServingGroupStatus(modelServingName types.NamespacedName, groupName string) ServingGroupStatus {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	if groups, exist := s.servingGroup[modelServingName]; exist {
		if group, ok := groups[groupName]; ok {
			return group.Status
		}
	}
	return ServingGroupNotFound
}

// DeleteModelServing delete modelServing in ServingGroup map
func (s *store) DeleteModelServing(modelServingName types.NamespacedName) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	delete(s.servingGroup, modelServingName)
}

// DeleteServingGroup delete ServingGroup in map
// Note: Revision history should be recorded using ControllerRevision before calling this method
// to ensure it's captured even if the deletion process fails.
func (s *store) DeleteServingGroup(modelServingName types.NamespacedName, groupName string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if groups, ok := s.servingGroup[modelServingName]; ok {
		delete(groups, groupName)
	}
}

// DeleteRole deletes a specific role from an ServingGroup
func (s *store) DeleteRole(modelServingName types.NamespacedName, groupName, roleName, roleID string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	servingGroups, ok := s.servingGroup[modelServingName]
	if !ok {
		return
	}

	servingGroup, ok := servingGroups[groupName]
	if !ok {
		return
	}

	roleMap, ok := servingGroup.roles[roleName]
	if !ok {
		return
	}
	delete(roleMap, roleID)
}

// AddServingGroup add ServingGroup item of one modelServing
func (s *store) AddServingGroup(modelServingName types.NamespacedName, idx int, revision string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	newGroup := &ServingGroup{
		Name:        utils.GenerateServingGroupName(modelServingName.Name, idx),
		runningPods: make(map[string]struct{}),
		Status:      ServingGroupCreating,
		Revision:    revision,
		roles:       make(map[string]map[string]*Role),
	}

	if _, ok := s.servingGroup[modelServingName]; !ok {
		s.servingGroup[modelServingName] = make(map[string]*ServingGroup)
	}
	s.servingGroup[modelServingName][newGroup.Name] = newGroup
}

// AddRole adds a new role to an ServingGroup
func (s *store) AddRole(modelServingName types.NamespacedName, groupName, roleName, roleID, revision string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	newRole := &Role{
		Name:     roleID,
		Status:   RoleCreating,
		Revision: revision,
	}

	if _, ok := s.servingGroup[modelServingName]; !ok {
		s.servingGroup[modelServingName] = make(map[string]*ServingGroup)
	}

	group, ok := s.servingGroup[modelServingName][groupName]
	if !ok {
		group = &ServingGroup{
			Name:        groupName,
			runningPods: make(map[string]struct{}),
			Status:      ServingGroupCreating,
			Revision:    revision,
			roles:       make(map[string]map[string]*Role),
		}
		s.servingGroup[modelServingName][groupName] = group
	}

	if _, exists := group.roles[roleName]; !exists {
		group.roles[roleName] = make(map[string]*Role)
	}

	group.roles[roleName][roleID] = newRole
}

// AddRunningPodToServingGroup add ServingGroup in runningPodOfServingGroup map
func (s *store) AddRunningPodToServingGroup(modelServingName types.NamespacedName, servingGroupName, runningPodName, revision, roleName, roleID string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if _, ok := s.servingGroup[modelServingName]; !ok {
		// If modelServingName not exist, create a new one
		s.servingGroup[modelServingName] = make(map[string]*ServingGroup)
	}

	group, ok := s.servingGroup[modelServingName][servingGroupName]
	if !ok {
		// If ServingGroupName not exist, create a new one
		group = &ServingGroup{
			Name:        servingGroupName,
			runningPods: map[string]struct{}{},
			Status:      ServingGroupCreating,
			Revision:    revision,
			roles:       make(map[string]map[string]*Role),
		}

		s.servingGroup[modelServingName][servingGroupName] = group
	}

	group.runningPods[runningPodName] = struct{}{} // runningPods map has been initialized during AddServingGroup.

	// Check if roleName exists, and initialize it if not
	if _, ok = group.roles[roleName]; !ok {
		group.roles[roleName] = make(map[string]*Role)
	}

	if _, ok = group.roles[roleName][roleID]; !ok {
		role := &Role{
			Name:     roleID,
			Status:   RoleCreating,
			Revision: revision,
		}
		group.roles[roleName][roleID] = role
	}
}

// AddServingGroupAndRole adds ServingGroup and roles if not exist
func (s *store) AddServingGroupAndRole(modelServingName types.NamespacedName, servingGroupName, revision, roleName, roleID string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if _, ok := s.servingGroup[modelServingName]; !ok {
		// If modelServingName not exist, create a new one
		s.servingGroup[modelServingName] = make(map[string]*ServingGroup)
	}

	group, ok := s.servingGroup[modelServingName][servingGroupName]
	if !ok {
		// If ServingGroupName not exist, create a new one
		group = &ServingGroup{
			Name:        servingGroupName,
			runningPods: map[string]struct{}{},
			Status:      ServingGroupCreating,
			Revision:    revision,
			roles:       make(map[string]map[string]*Role),
		}

		s.servingGroup[modelServingName][servingGroupName] = group
	}

	// Check if roleName exists, and initialize it if not
	if _, ok = group.roles[roleName]; !ok {
		group.roles[roleName] = make(map[string]*Role)
	}

	if _, ok = group.roles[roleName][roleID]; !ok {
		role := &Role{
			Name:     roleID,
			Status:   RoleCreating,
			Revision: revision,
		}
		group.roles[roleName][roleID] = role
	}
}

// DeleteRunningPodFromServingGroup delete runningPod in map
func (s *store) DeleteRunningPodFromServingGroup(modelServingName types.NamespacedName, servingGroupName string, pod string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if groups, exist := s.servingGroup[modelServingName]; exist {
		if group, ok := groups[servingGroupName]; ok {
			delete(group.runningPods, pod)
		}
	}
}

// UpdateServingGroupStatus update status of one ServingGroup
func (s *store) UpdateServingGroupStatus(modelServingName types.NamespacedName, groupName string, status ServingGroupStatus) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	groups, ok := s.servingGroup[modelServingName]
	if !ok {
		return fmt.Errorf("failed to find modelServing %s", modelServingName.Namespace+"/"+modelServingName.Name)
	}
	if group, ok := groups[groupName]; ok {
		group.Status = status
		groups[groupName] = group
	} else {
		return fmt.Errorf("failed to find ServingGroup %s in modelServing %s", groupName, modelServingName.Namespace+"/"+modelServingName.Name)
	}
	return nil
}
