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

	_, err = l.writer.Write([]byte(output + "\n"))
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
	return string(data), nil
}

// formatText formats the entry as structured text
func (l *accessLoggerImpl) formatText(entry *AccessLogEntry) (string, error) {
	// Format: [timestamp] "METHOD /path PROTOCOL" status_code [error=type:message]
	// model_name=name model_route=route model_server=server selected_pod=pod request_id=id tokens=input/output
	// timings=total(req+upstream+resp)ms

	timestamp := entry.Timestamp.Format(time.RFC3339Nano)

	// Basic request line with status code
	line := fmt.Sprintf(`[%s] "%s %s %s" %d`,
		timestamp, entry.Method, entry.Path, entry.Protocol,
		entry.StatusCode)

	// Add error information immediately after status code
	if entry.Error != nil {
		line += fmt.Sprintf(" error=%s:%s", entry.Error.Type, entry.Error.Message)
	}

	// Add AI-specific fields
	if entry.ModelName != "" {
		line += fmt.Sprintf(" model_name=%s", entry.ModelName)
	}
	if entry.ModelRoute != "" {
		line += fmt.Sprintf(" model_route=%s", entry.ModelRoute)
	}
	if entry.ModelServer != "" {
		line += fmt.Sprintf(" model_server=%s", entry.ModelServer)
	}

	// Add Gateway API fields
	if entry.Gateway != "" {
		line += fmt.Sprintf(" gateway=%s", entry.Gateway)
	}
	if entry.HTTPRoute != "" {
		line += fmt.Sprintf(" http_route=%s", entry.HTTPRoute)
	}
	if entry.InferencePool != "" {
		line += fmt.Sprintf(" inference_pool=%s", entry.InferencePool)
	}

	if entry.SelectedPod != "" {
		line += fmt.Sprintf(" selected_pod=%s", entry.SelectedPod)
	}
	if entry.RequestID != "" {
		line += fmt.Sprintf(" request_id=%s", entry.RequestID)
	}

	// Add token information
	if entry.InputTokens > 0 || entry.OutputTokens > 0 {
		line += fmt.Sprintf(" tokens=%d/%d", entry.InputTokens, entry.OutputTokens)
	}

	// Add complete timing breakdown with total and breakdown
	line += fmt.Sprintf(" timings=%dms(%d+%d+%d)",
		entry.DurationTotal,
		entry.DurationRequestProcessing,
		entry.DurationUpstreamProcessing,
		entry.DurationResponseProcessing)

	return line, nil
}

// noopAccessLogger is a no-op implementation when logging is disabled
type noopAccessLogger struct{}

func (l *noopAccessLogger) Log(entry *AccessLogEntry) error {
	return nil
}

func (l *noopAccessLogger) Close() error {
	return nil
}
