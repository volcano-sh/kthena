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
)

// ExportedRole is a DTO used exclusively for JSON serialization of the cache state.
// It allows us to safely export unexported inner structures without exposing internal lock mechanics.
type ExportedRole struct {
	Name             string     `json:"name"`
	Revision         string     `json:"revision"`
	RoleTemplateHash string     `json:"roleTemplateHash"`
	Status           RoleStatus `json:"status"`
}

// ExportedServingGroup is a DTO used exclusively for JSON serialization of the cache state.
// It maps the internal non-exported fields (like runningPods and roles) to JSON serializable formats.
type ExportedServingGroup struct {
	Name        string             `json:"name"`
	RunningPods []string           `json:"runningPods"`
	Revision    string             `json:"revision"`
	Status      ServingGroupStatus `json:"status"`
	// Roles maps role name -> role ID -> ExportedRole
	Roles map[string]map[string]ExportedRole `json:"roles"`
}

// DumpCache returns a JSON dump of the current store cache representation
func (s *store) DumpCache() ([]byte, error) {
	exportedCache := make(map[string]map[string]ExportedServingGroup)

	s.mutex.RLock()
	for msName, groups := range s.servingGroup {
		msKey := msName.String()
		exportedCache[msKey] = make(map[string]ExportedServingGroup)
		for groupName, group := range groups {
			expGroup := ExportedServingGroup{
				Name:        group.Name,
				Revision:    group.Revision,
				Status:      group.Status,
				RunningPods: make([]string, 0, len(group.runningPods)),
				Roles:       make(map[string]map[string]ExportedRole),
			}
			for pod := range group.runningPods {
				expGroup.RunningPods = append(expGroup.RunningPods, pod)
			}
			for roleName, roleMap := range group.roles {
				expGroup.Roles[roleName] = make(map[string]ExportedRole)
				for roleID, role := range roleMap {
					expGroup.Roles[roleName][roleID] = ExportedRole{
						Name:             role.Name,
						Revision:         role.Revision,
						RoleTemplateHash: role.RoleTemplateHash,
						Status:           role.Status,
					}
				}
			}
			exportedCache[msKey][groupName] = expGroup
		}
	}
	s.mutex.RUnlock()

	data, err := json.Marshal(exportedCache)
	if err != nil {
		return nil, err
	}
	return data, nil
}
