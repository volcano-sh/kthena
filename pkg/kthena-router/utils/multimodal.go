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

package utils

import (
	"github.com/cespare/xxhash"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

// mediaPart describes a content part type that carries media: its modality and
// the part fields that may hold the media source, in lookup order.
type mediaPart struct {
	modality common.Modality
	fields   []string
}

// mediaParts maps content part types to the media they carry. It covers the
// OpenAI Chat Completions and Responses APIs, the audio_url and video_url
// extensions shared by vLLM and SGLang, and the Anthropic Messages API (image).
var mediaParts = map[string]mediaPart{
	"image_url":   {modality: common.ModalityImage, fields: []string{"image_url"}},
	"input_image": {modality: common.ModalityImage, fields: []string{"image_url", "file_id"}},
	"image":       {modality: common.ModalityImage, fields: []string{"source"}},
	"input_audio": {modality: common.ModalityAudio, fields: []string{"input_audio"}},
	"audio_url":   {modality: common.ModalityAudio, fields: []string{"audio_url"}},
	"video_url":   {modality: common.ModalityVideo, fields: []string{"video_url"}},
}

// parseMediaPart returns the media item of a content part. It returns false if
// the part type does not carry media or the part has no media source.
func parseMediaPart(partType string, part map[string]interface{}) (common.MultiModalItem, bool) {
	media, ok := mediaParts[partType]
	if !ok {
		return common.MultiModalItem{}, false
	}
	for _, field := range media.fields {
		if source := mediaSource(part[field]); source != "" {
			return common.MultiModalItem{Modality: media.modality, Hash: xxhash.Sum64String(source)}, true
		}
	}
	return common.MultiModalItem{}, false
}

// mediaSource returns the URL or the inline payload held by a media field. The
// field is either the source string itself or an object holding it, such as
// {"url": "..."}, {"data": "...", "format": "wav"} or the Anthropic
// {"type": "base64", "media_type": "image/png", "data": "..."}.
func mediaSource(field interface{}) string {
	switch v := field.(type) {
	case string:
		return v
	case map[string]interface{}:
		for _, key := range []string{"url", "data", "file_id"} {
			if source, ok := v[key].(string); ok && source != "" {
				return source
			}
		}
	}
	return ""
}
