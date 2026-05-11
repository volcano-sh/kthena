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

import "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"

// Factory creates KV connectors based on type
type Factory struct {
	connectors map[v1alpha1.KVConnectorType]func() KVConnector
}

// NewFactory creates a new connector factory
func NewFactory() *Factory {
	return &Factory{
		connectors: make(map[v1alpha1.KVConnectorType]func() KVConnector),
	}
}

// RegisterConnector registers a connector with the factory
func (f *Factory) RegisterConnectorBuilder(connectorType v1alpha1.KVConnectorType, constructor func() KVConnector) {
	f.connectors[connectorType] = constructor
}

// GetConnector returns a connector by type
func (f *Factory) GetConnector(connectorType v1alpha1.KVConnectorType) KVConnector {
	connector, ok := f.connectors[connectorType]
	if !ok {
		return NewHTTPConnector() // Default to HTTP connector if not found
	}
	return connector()
}

// NewDefaultFactory returns a factory with all default connectors registered
func NewDefaultFactory() *Factory {
	factory := NewFactory()

	// Register default connectors
	factory.RegisterConnectorBuilder(v1alpha1.ConnectorTypeHTTP, NewHTTPConnector)
	factory.RegisterConnectorBuilder(v1alpha1.ConnectorTypeLMCache, NewHTTPConnector)      // LMCache uses HTTP connector for now
	factory.RegisterConnectorBuilder(v1alpha1.ConnectorTypeMoonCake, NewMoonCakeConnector) // MoonCakeConnector in vllm-ascend
	factory.RegisterConnectorBuilder(v1alpha1.ConnectorTypeNIXL, NewNIXLConnector)
	factory.RegisterConnectorBuilder(ConnectorTypeSGLang, NewSGLangConnector) // SGLang disaggregated prefill-decode (internal, not user-configurable)

	return factory
}
