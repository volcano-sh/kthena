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

package connectors

// KVTransferParams holds parameters for Key-Value cache transfer between generation steps.
type KVTransferParams struct {
	DoRemoteDecode  bool     `json:"do_remote_decode"`
	DoRemotePrefill bool     `json:"do_remote_prefill"`
	RemoteEngineID  *string  `json:"remote_engine_id,omitempty"`
	RemoteBlockIDs  []string `json:"remote_block_ids,omitempty"`
	RemoteHost      *string  `json:"remote_host,omitempty"`
	RemotePort      *int     `json:"remote_port,omitempty"`
}

// OnFlightHooks carries per-request callbacks that the router passes into
// Proxy() so connectors can track in-flight request counts on prefill and
// decode pods at the precise point in their proxy lifecycle.
// All fields are optional; a nil pointer or nil field is a no-op.
type OnFlightHooks struct {
	IncrPrefill func()
	DecrPrefill func()
	IncrDecode  func()
	DecrDecode  func()
}
