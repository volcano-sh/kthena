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

package accesslog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// AccessLogger defines the interface for logging access entries
type AccessLogger interface {
	Log(entry *AccessLogEntry) error
	Close() error
}

// LogFormat represents the output format for access logs
type LogFormat string

const (
	// FormatJSON outputs logs in JSON format
	FormatJSON LogFormat = "json"
	// FormatText outputs logs in structured text format
	FormatText LogFormat = "text"
)

// AccessLoggerConfig contains configuration for access logging
type AccessLoggerConfig struct {
	// Format specifies the log output format (json or text)
	Format LogFormat `json:"format" yaml:"format"`
	// Output specifies where to write logs ("stdout", "stderr", or file path)
	Output string `json:"output" yaml:"output"`
	// Enabled controls whether access logging is enabled
	Enabled bool `json:"enabled" yaml:"enabled"`
}

// DefaultAccessLoggerConfig returns default configuration
func DefaultAccessLoggerConfig() *AccessLoggerConfig {
	return &AccessLoggerConfig{
		Format:  FormatJSON,
		Output:  "stdout",
		Enabled: true,
	}
}

// accessLoggerImpl implements AccessLogger interface
type accessLoggerImpl struct {
	config *AccessLoggerConfig
	writer io.WriteCloser
}

// NewAccessLogger creates a new access logger with the given configuration
func NewAccessLogger(config *AccessLoggerConfig) (AccessLogger, error) {
	if config == nil {
		config = DefaultAccessLoggerConfig()
	}

	if !config.Enabled {
		return &noopAccessLogger{}, nil
	}

	var writer io.WriteCloser
	switch config.Output {
	case "stdout", "":
		writer = os.Stdout
	case "stderr":
		writer = os.Stderr
	default:
		// File output
		file, err := os.OpenFile(config.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open access log file %s: %w", config.Output, err)
		}
		writer = file
	}

	return &accessLoggerImpl{
		config: config,
		writer: writer,
	}, nil
}

// Log writes an access log entry
func (l *accessLoggerImpl) Log(entry *AccessLogEntry) error {
	if entry == nil {
		return nil
	}

	var output string
	var err error

	switch l.config.Format {
	case FormatJSON:
		output, err = l.formatJSON(entry)
	case FormatText:
		output, err = l.formatText(entry)
	default:
		return fmt.Errorf("unsupported log format: %s", l.config.Format)
	}

	if err != nil {
		return fmt.Errorf("failed to format access log entry: %w", err)
	}

	_, err = fmt.Fprintf(l.writer, "%s\n", output)
	if err != nil {
		return fmt.Errorf("failed to write access log entry: %w", err)
	}

	return nil
}

// Close closes the access logger
func (l *accessLoggerImpl) Close() error {
	if l.writer != os.Stdout && l.writer != os.Stderr {
		return l.writer.Close()
	}
	return nil
}

// formatJSON formats the entry as JSON
func (l *accessLoggerImpl) formatJSON(entry *AccessLogEntry) (string, error) {
	data, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	if len(entry.ClaimFields) == 0 {
		return string(data), nil
	}

	// Append the claim fields inside the closing brace. Their names are
	// validated not to collide with the fields marshaled above.
	var line strings.Builder
	line.Write(data[:len(data)-1])
	for _, field := range entry.ClaimFields {
		name, err := json.Marshal(field.Name)
		if err != nil {
			return "", err
		}
		value, err := json.Marshal(field.Value)
		if err != nil {
			return "", err
		}
		line.WriteByte(',')
		line.Write(name)
		line.WriteByte(':')
		line.Write(value)
	}
	line.WriteByte('}')
	return line.String(), nil
}

// writeTextField appends " key=value" to line, skipping empty values. A value
// holding a space, a quote or any control character is quoted, so one entry
// stays one parseable line and escape sequences cannot reach a terminal.
func writeTextField(line *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	if needsQuoting(value) {
		fmt.Fprintf(line, " %s=%q", key, value)
		return
	}
	fmt.Fprintf(line, " %s=%s", key, value)
}

func needsQuoting(value string) bool {
	return strings.ContainsAny(value, " \"") ||
		strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// formatText formats the entry as structured text
func (l *accessLoggerImpl) formatText(entry *AccessLogEntry) (string, error) {
	// Format: [timestamp] "METHOD /path PROTOCOL" status_code [error=type:message]
	// model_name=name model_route=route model_server=server selected_pod=pod request_id=id tokens=input/output
	// timings=total(req+upstream+resp)ms

	timestamp := entry.Timestamp.Format(time.RFC3339Nano)

	var line strings.Builder

	// Basic request line with status code
	fmt.Fprintf(&line, "[%s] %q %d",
		timestamp, entry.Method+" "+entry.Path+" "+entry.Protocol,
		entry.StatusCode)

	// Add error information immediately after status code
	if entry.Error != nil {
		writeTextField(&line, "error", entry.Error.Type+":"+entry.Error.Message)
	}

	// Add AI-specific fields
	writeTextField(&line, "model_name", entry.ModelName)
	writeTextField(&line, "model_route", entry.ModelRoute)
	writeTextField(&line, "model_server", entry.ModelServer)
	writeTextField(&line, "selected_pod", entry.SelectedPod)
	writeTextField(&line, "request_id", entry.RequestID)
	writeTextField(&line, "backend_type", entry.BackendType)
	writeTextField(&line, "backend_name", entry.BackendName)
	writeTextField(&line, "upstream_model", entry.UpstreamModel)
	if entry.UpstreamStatusCode > 0 {
		fmt.Fprintf(&line, " upstream_status_code=%d", entry.UpstreamStatusCode)
	}
	if entry.UpstreamAttempts > 0 {
		fmt.Fprintf(&line, " upstream_attempts=%d", entry.UpstreamAttempts)
	}
	writeTextField(&line, "error_origin", entry.ErrorOrigin)

	// Add Gateway API / Inference Extension fields (if present)
	writeTextField(&line, "gateway", entry.Gateway)
	writeTextField(&line, "http_route", entry.HTTPRoute)
	writeTextField(&line, "inference_pool", entry.InferencePool)

	// Add token information
	if entry.InputTokens > 0 || entry.OutputTokens > 0 {
		fmt.Fprintf(&line, " tokens=%d/%d", entry.InputTokens, entry.OutputTokens)
	}
	if entry.UpstreamInputTokens != nil {
		fmt.Fprintf(&line, " upstream_input_tokens=%d", *entry.UpstreamInputTokens)
	}
	if entry.CachedInputTokens != nil {
		fmt.Fprintf(&line, " cached_input_tokens=%d", *entry.CachedInputTokens)
	}

	// Claim values come from the client's token, so they go through
	// writeTextField like every other string value.
	for _, field := range entry.ClaimFields {
		writeTextField(&line, field.Name, field.Value)
	}

	// Add complete timing breakdown with total and breakdown
	fmt.Fprintf(&line, " timings=%dms(%d+%d+%d)",
		entry.DurationTotal,
		entry.DurationRequestProcessing,
		entry.DurationUpstreamProcessing,
		entry.DurationResponseProcessing)

	return line.String(), nil
}

// noopAccessLogger is a no-op implementation when logging is disabled
type noopAccessLogger struct{}

func (l *noopAccessLogger) Log(entry *AccessLogEntry) error {
	return nil
}

func (l *noopAccessLogger) Close() error {
	return nil
}
