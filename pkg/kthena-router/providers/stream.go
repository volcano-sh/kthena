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

package providers

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"
)

// ForwardStream reads newline-delimited stream events from body and forwards them
// to c's streaming response, using parser to extract usage as each line is read
// and, if forwarded, written. A line is dropped instead of forwarded when
// parser reports SuppressLine (used for a usage-only event the router itself
// requested on the client's behalf). onUsage, if non-nil, is invoked at most
// once, synchronously, after the stream ends: with the final usage
// parser.FinalStreamUsage reports or, failing that, with the last usage a
// per-line parse found. Per-line usage is treated as a cumulative snapshot,
// since OpenAI-compatible backends may attach running totals to every chunk,
// so earlier copies are superseded rather than reported.
//
// It returns the completion-token count from the final usage parser.FinalStreamUsage
// reports, and any streaming error. A client disconnect before parser reports the
// stream complete is surfaced as context.Canceled, matching a genuine read error;
// a disconnect after completion (or when the parser does not track completion) is
// not treated as an error.
func ForwardStream(c *gin.Context, body io.Reader, parser ResponseUsageParser, onUsage func(TokenUsage)) (int, error) {
	reader := bufio.NewReader(body)
	var streamErr error
	var latestUsage TokenUsage
	hasLatestUsage := false
	clientDisconnected := c.Stream(func(w io.Writer) bool {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			parseResult := parser.ParseStreamLine(string(line))
			if parseResult.HasUsage {
				klog.V(4).Infof("Parsed usage: %+v", parseResult.Usage)
				latestUsage, hasLatestUsage = parseResult.Usage, true
				if parseResult.SuppressLine {
					return true
				}
			}
			n, writeErr := w.Write(line)
			if writeErr != nil {
				klog.Errorf("error writing stream body: %v", writeErr)
				streamErr = writeErr
				return false
			}
			if n != len(line) {
				klog.Errorf("error writing stream body: %v", io.ErrShortWrite)
				streamErr = io.ErrShortWrite
				return false
			}
			parser.RecordStreamLineWritten(string(line))
		}
		if err != nil {
			if err != io.EOF {
				if !errors.Is(err, context.Canceled) || !parser.StreamCompleted() {
					klog.Errorf("error reading stream body: %v", err)
					streamErr = err
				}
			}
			return false
		}
		return true
	})
	if clientDisconnected && streamErr == nil && !parser.StreamCompleted() {
		streamErr = context.Canceled
	}

	totalTokens := 0
	usage, ok := parser.FinalStreamUsage()
	if ok {
		totalTokens = usage.CompletionTokens
	} else if hasLatestUsage {
		// The parser reports usage per line (OpenAI chat/completions) rather
		// than at a terminal event, or the stream was cut off: account for the
		// last usage read so it is neither lost nor counted once per chunk.
		usage, ok = latestUsage, true
	}
	if ok {
		klog.V(4).Infof("Final usage: %+v", usage)
		if onUsage != nil {
			onUsage(usage)
		}
	}
	return totalTokens, streamErr
}

// ForwardBody copies a non-streaming response body to c.Writer, using parser to
// extract usage from the completed body. onUsage, if non-nil, is invoked with the
// parsed usage. It returns the completion-token count and any copy error.
func ForwardBody(c *gin.Context, body io.Reader, parser ResponseUsageParser, onUsage func(TokenUsage)) (int, error) {
	var buf bytes.Buffer
	teeReader := io.TeeReader(body, &buf)
	if _, err := io.Copy(c.Writer, teeReader); err != nil {
		klog.Errorf("copy response to downstream failed: %v", err)
		return 0, err
	}

	if usage, ok := parser.ParseBody(buf.Bytes()); ok {
		klog.V(4).Infof("Parsed usage: %+v", usage)
		if onUsage != nil {
			onUsage(usage)
		}
		return usage.CompletionTokens, nil
	}
	return 0, nil
}
