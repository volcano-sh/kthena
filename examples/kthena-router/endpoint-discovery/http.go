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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"k8s.io/apimachinery/pkg/types"

	api "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/discovery"
)

const maxSnapshotBytes = 1 << 20

type httpSource struct{ url string }

func newHTTPSource(_ types.NamespacedName, parameters map[string]interface{}, _ func()) (discovery.Source, error) {
	address, ok := parameters["url"].(string)
	u, err := url.Parse(address)
	if len(parameters) != 1 || !ok || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("http-json requires exactly one parameter: an absolute http(s) url")
	}
	// Polling only: the controller owns scheduling; construction performs no I/O.
	return &httpSource{url: address}, nil
}

func (s *httpSource) Snapshot(ctx context.Context) ([]api.Endpoint, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxSnapshotBytes {
		return nil, fmt.Errorf("registry snapshot exceeds %d bytes", maxSnapshotBytes)
	}
	var endpoints []api.Endpoint
	if err := json.Unmarshal(body, &endpoints); err != nil {
		return nil, fmt.Errorf("decode registry snapshot: %w", err)
	}
	if endpoints == nil {
		return nil, fmt.Errorf("registry must return an array; use [] for an empty snapshot")
	}
	return endpoints, nil
}

// Requests use the shared HTTP transport; this polling source owns no worker
// or connection pool. Each Snapshot closes its response body before returning.
func (*httpSource) Close() {}
