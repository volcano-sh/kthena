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

package webhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestValidPodNameLength(t *testing.T) {
	replicas := int32(3)
	type args struct {
		ms *workloadv1alpha1.ModelServing
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{
		{
			name: "normal name length",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					ObjectMeta: v1.ObjectMeta{
						Name: "valid-name",
					},
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: &replicas, WorkerReplicas: 2},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "name length exceeds limit",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					ObjectMeta: v1.ObjectMeta{
						Name: "this-is-a-very-long-name-that-exceeds-the-allowed-length-for-generated-name",
					},
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: &replicas, WorkerReplicas: 2},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("metadata").Child("name"),
					"this-is-a-very-long-name-that-exceeds-the-allowed-length-for-generated-name",
					"invalid name: must be no more than 63 characters"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validGeneratedNameLength(tt.args.ms)
			if got != nil {
				assert.EqualValues(t, tt.want[0], got[0])
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestValidateRollingUpdateConfiguration(t *testing.T) {
	replicas := int32(3)
	type args struct {
		ms *workloadv1alpha1.ModelServing
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{
		{
			name: "normal rolling update configuration",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								MaxSurge: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "invalid maxUnavailable format",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: intstr.IntOrString{
									Type:   intstr.String,
									StrVal: "invalid",
								},
								MaxSurge: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("maxUnavailable"),
					intstr.IntOrString{
						Type:   intstr.String,
						StrVal: "invalid",
					},
					"a valid percent string must be a numeric string followed by an ending '%' (e.g. '1%',  or '93%', regex used for validation is '[0-9]+%')",
				),
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("maxUnavailable"),
					intstr.IntOrString{
						Type:   intstr.String,
						StrVal: "invalid",
					},
					"validate maxUnavailable",
				),
			},
		},
		{
			name: "invalid maxSurge format",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								MaxSurge: intstr.IntOrString{
									Type:   intstr.String,
									StrVal: "invalid",
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("maxSurge"),
					intstr.IntOrString{
						Type:   intstr.String,
						StrVal: "invalid",
					},
					"a valid percent string must be a numeric string followed by an ending '%' (e.g. '1%',  or '93%', regex used for validation is '[0-9]+%')",
				),
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("maxSurge"),
					intstr.IntOrString{
						Type:   intstr.String,
						StrVal: "invalid",
					},
					"validate maxSurge",
				),
			},
		},
		{
			name: "both maxUnavailable and maxSurge are zero",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 0,
								},
								MaxSurge: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 0,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration"),
					"",
					"maxUnavailable and maxSurge cannot both be 0",
				),
			},
		},
		{
			name: "valid partition - within range",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								MaxSurge: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								Partition: int32Ptr(1),
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "invalid partition - negative value",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								MaxSurge: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								Partition: int32Ptr(-1),
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("partition"),
					int32(-1),
					"partition must be greater than or equal to 0",
				),
			},
		},
		{
			name: "invalid partition - equal to replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								MaxSurge: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								Partition: int32Ptr(3),
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("partition"),
					int32(3),
					"partition must be less than replicas (3)",
				),
			},
		},
		{
			name: "invalid partition - greater than replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								MaxSurge: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								Partition: int32Ptr(5),
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("partition"),
					int32(5),
					"partition must be less than replicas (3)",
				),
			},
		},
		{
			name: "valid partition - zero value",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								MaxSurge: intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								Partition: int32Ptr(0),
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateRollingUpdateConfiguration(tt.args.ms)
			if got != nil {
				assert.EqualValues(t, tt.want, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestValidatorReplicas(t *testing.T) {
	type args struct {
		ms *workloadv1alpha1.ModelServing
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{
		{
			name: "normal replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32Ptr(3),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: int32Ptr(2), WorkerReplicas: 1},
								{Name: "role2", Replicas: int32Ptr(1), WorkerReplicas: 1},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "replicas is nil",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32PtrNil(),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: int32Ptr(2), WorkerReplicas: 1},
								{Name: "role2", Replicas: int32Ptr(1), WorkerReplicas: 1},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("replicas"),
					int32PtrNil(),
					"replicas must be a positive integer",
				),
			},
		},
		{
			name: "replicas is less than 0",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32Ptr(-1),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: int32Ptr(2), WorkerReplicas: 1},
								{Name: "role2", Replicas: int32Ptr(1), WorkerReplicas: 1},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("replicas"),
					int32Ptr(-1),
					"replicas must be a positive integer",
				),
			},
		},
		{
			name: "role replicas is less than 0",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32Ptr(3),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: int32Ptr(-1), WorkerReplicas: 1},
								{Name: "role2", Replicas: int32Ptr(1), WorkerReplicas: 1},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(0).Child("replicas"),
					int32Ptr(-1),
					"role replicas must be a positive integer",
				),
			},
		},
		{
			name: "role replicas is nil",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32Ptr(3),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: int32PtrNil(), WorkerReplicas: 1},
								{Name: "role2", Replicas: int32Ptr(1), WorkerReplicas: 1},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(0).Child("replicas"),
					int32PtrNil(),
					"role replicas must be a positive integer",
				),
			},
		},
		{
			name: "no role",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32Ptr(3),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles"),
					[]workloadv1alpha1.Role{},
					"roles must be specified",
				),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validatorReplicas(tt.args.ms)
			if got != nil {
				assert.EqualValues(t, tt.want, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestValidateGangPolicy(t *testing.T) {
	replicas := int32(3)
	roleReplicas := int32(2)
	type args struct {
		ms *workloadv1alpha1.ModelServing
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{
		{
			name: "valid minRoleReplicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: &workloadv1alpha1.GangPolicy{
								MinRoleReplicas: map[string]int32{
									"worker": 3, // 2*1 (entry) + 3 (workers) = 5 total, min=3 is valid
								},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "invalid minRoleReplicas - role not exist",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: &workloadv1alpha1.GangPolicy{
								MinRoleReplicas: map[string]int32{
									"nonexistent": 1,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("gangPolicy").Child("minRoleReplicas").Key("nonexistent"),
					"nonexistent",
					"role nonexistent does not exist in template.roles",
				),
			},
		},
		{
			name: "invalid minRoleReplicas - exceeds total replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: &workloadv1alpha1.GangPolicy{
								MinRoleReplicas: map[string]int32{
									"worker": 10, // 2*1 (entry) + 3 (workers) = 5 total, min=10 is invalid
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("gangPolicy").Child("minRoleReplicas").Key("worker"),
					int32(10),
					"minRoleReplicas (10) for role worker cannot exceed total replicas (5)",
				),
			},
		},
		{
			name: "invalid minRoleReplicas - negative value",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: &workloadv1alpha1.GangPolicy{
								MinRoleReplicas: map[string]int32{
									"worker": -1,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("gangPolicy").Child("minRoleReplicas").Key("worker"),
					int32(-1),
					"minRoleReplicas for role worker must be non-negative",
				),
			},
		},
		{
			name: "nil gang Policy",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: nil,
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "nil minRoleReplicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: &workloadv1alpha1.GangPolicy{
								MinRoleReplicas: nil,
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateGangPolicy(tt.args.ms)
			if got != nil {
				assert.EqualValues(t, tt.want, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestValidateWorkerReplicas(t *testing.T) {
	replicas := int32(3)
	roleReplicas := int32(2)
	type args struct {
		ms *workloadv1alpha1.ModelServing
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{
		{
			name: "valid worker replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "valid zero worker replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 0,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "invalid negative worker replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: -1,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(0).Child("workerReplicas"),
					int32(-1),
					"workerReplicas must be a non-negative integer",
				),
			},
		},
		{
			name: "multiple roles with one invalid worker replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker1",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
								{
									Name:           "worker2",
									Replicas:       &roleReplicas,
									WorkerReplicas: -1,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(1).Child("workerReplicas"),
					int32(-1),
					"workerReplicas must be a non-negative integer",
				),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateWorkerReplicas(tt.args.ms)
			if got != nil {
				assert.EqualValues(t, tt.want, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func int32Ptr(i int32) *int32 {
	return &i
}

func int32PtrNil() *int32 {
	return nil
}
