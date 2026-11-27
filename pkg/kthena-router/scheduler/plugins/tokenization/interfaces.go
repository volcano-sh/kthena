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

package tokenization

import "context"

// Tokenizer provides basic text tokenization functionality for converting text to tokens
type Tokenizer interface {
	TokenizeInputText(string) ([]byte, error)
}

// ExtendedTokenizer extends the basic Tokenizer with advanced features like
// context support, chat templates, and configurable options
type ExtendedTokenizer interface {
	Tokenizer
	TokenizeWithOptions(ctx context.Context, input TokenizeInput) (*TokenizeResult, error)
}

type remoteTokenizer interface {
	ExtendedTokenizer
	GetEndpoint() string
	Close() error
}

type engineAdapter interface {
	PrepareTokenizeRequest(input TokenizeInput) (interface{}, error)
	ParseTokenizeResponse(data []byte) (*TokenizeResult, error)
	GetTokenizePath(inputType TokenizeInputType) string
}
