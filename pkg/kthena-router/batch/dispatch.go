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

package batch

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

const (
	// defaultLineTimeout bounds one line, from sending it to the last byte of its answer.
	defaultLineTimeout = 10 * time.Minute
	// defaultMaxResponse is the largest answer one line keeps; a larger one is never cut short into the output.
	defaultMaxResponse = 2 << 20
)

// Identity is who a batch line runs as and how its batch reached the router.
type Identity struct {
	Tenant  string
	Subject string
	Origin  Origin
}

type identityKey struct{}

// WithIdentity puts the identity of a batch line into a request context; only the worker does this.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

// IdentityFromContext returns the identity of a batch line; it lives in the Go context, so no network request can carry it.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityKey{}).(Identity)
	return identity, ok
}

// HandlerDispatcher sends each line through an in-process handler, so a line takes the same path as a user request.
type HandlerDispatcher struct {
	handler     http.Handler
	maxResponse int
	lineTimeout time.Duration
}

var _ Dispatcher = (*HandlerDispatcher)(nil)

// NewHandlerDispatcher sends batch lines to handler, which is never bound to a port.
func NewHandlerDispatcher(handler http.Handler) *HandlerDispatcher {
	return &HandlerDispatcher{handler: handler, maxResponse: defaultMaxResponse, lineTimeout: defaultLineTimeout}
}

// Dispatch runs one line and always returns an answer, even when the handler panics.
func (d *HandlerDispatcher) Dispatch(ctx context.Context, request *Request) (response *Response) {
	lineCtx, cancel := context.WithTimeout(ctx, d.lineTimeout)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			klog.Errorf("batch: request %s panicked: %v", request.RequestID, recovered)
			response = &Response{Err: fmt.Errorf("batch: the request panicked: %v", recovered)}
		}
	}()
	identity := Identity{Tenant: request.Tenant, Subject: request.Subject, Origin: request.Origin}
	req, err := http.NewRequestWithContext(WithIdentity(lineCtx, identity), http.MethodPost, request.Endpoint, bytes.NewReader(request.Body))
	if err != nil {
		return &Response{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", request.RequestID)
	// An HTTPRoute that lists hostnames matches a line only with the host its batch came in on.
	req.Host = request.Origin.Host
	writer := &lineWriter{header: http.Header{}, limit: d.maxResponse, done: lineCtx.Done()}
	d.handler.ServeHTTP(writer, req)
	switch {
	case writer.overflow:
		return &Response{StatusCode: writer.code(), Err: ErrResponseTooLarge}
	case writer.status == 0 && lineCtx.Err() != nil:
		return &Response{Err: lineCtx.Err()}
	}
	return &Response{StatusCode: writer.code(), Body: writer.body.Bytes()}
}

// lineWriter collects one answer in memory up to a limit, and acts enough like a connection for the router's code paths.
type lineWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	limit    int
	overflow bool
	done     <-chan struct{}
}

func (w *lineWriter) Header() http.Header { return w.header }

func (w *lineWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.overflow || w.body.Len()+len(p) > w.limit {
		w.overflow = true
		return 0, ErrResponseTooLarge
	}
	return w.body.Write(p)
}

// Flush is here because the router flushes on its streaming paths.
func (w *lineWriter) Flush() {}

// CloseNotify is here because gin's Stream needs it; the channel follows the line's context.
func (w *lineWriter) CloseNotify() <-chan bool {
	gone := make(chan bool, 1)
	go func() {
		<-w.done
		gone <- true
	}()
	return gone
}

func (w *lineWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
