/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package sessionsticky

import (
	"time"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

const defaultSessionAffinitySeconds int32 = 300

// TTL returns binding TTL from spec.sessionAffinitySeconds, or 5 minutes when unset.
func TTL(spec *networkingv1alpha1.SessionSticky) time.Duration {
	sec := defaultSessionAffinitySeconds
	if spec != nil && spec.SessionAffinitySeconds != nil && *spec.SessionAffinitySeconds >= 1 {
		sec = *spec.SessionAffinitySeconds
	}
	return time.Duration(sec) * time.Second
}
