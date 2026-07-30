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

package tokenizer

import (
	"fmt"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktokenloader "github.com/pkoukk/tiktoken-go-loader"
)

const encodingName = "cl100k_base"

var getBPE = sync.OnceValues(func() (*tiktoken.Tiktoken, error) {
	tiktoken.SetBpeLoader(tiktokenloader.NewOfflineLoader())
	return tiktoken.GetEncoding(encodingName)
})

type TickToken struct{}

func (t *TickToken) CalculateTokenNum(prompt string) (int, error) {
	encoding, err := getBPE()
	if err != nil {
		return 0, fmt.Errorf("failed to initialize BPE encoding: %w", err)
	}
	return len(encoding.Encode(prompt, nil, nil)), nil
}
