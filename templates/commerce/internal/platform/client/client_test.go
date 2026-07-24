package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(options ...func(*Config)) *Client {
	config := Config{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("unexpected test upstream request")
		})},
		TotalTimeout:   time.Second,
		AttemptTimeout: 100 * time.Millisecond,
		BaseBackoff:    time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Jitter:         func(delay time.Duration) time.Duration { return delay },
		Breaker:        BreakerConfig{FailureThreshold: 10, OpenFor: time.Second},
	}
	for _, option := range options {
		option(&config)
	}
	return New(config)
}

func TestClientRetriesIdempotentRequestOnly(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(func(config *Config) {
		config.HTTPClient = newHandlerHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				http.Error(w, "temporary", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
	})

	req, err := http.NewRequest(http.MethodPost, "http://upstream.test/checkout", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "checkout-1")
	response, err := client.Do(context.Background(), req, Policy{MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d, want status=%d calls=2", response.StatusCode, calls.Load(), http.StatusNoContent)
	}
}

func TestClientDoesNotRetryUnsafeRequestWithoutIdempotencyKey(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(func(config *Config) {
		config.HTTPClient = newHandlerHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, "temporary", http.StatusServiceUnavailable)
		}))
	})

	req, err := http.NewRequest(http.MethodPost, "http://upstream.test/checkout", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), req, Policy{MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want 1", calls.Load())
	}
}

func TestClientReplaysIdempotentRequestBody(t *testing.T) {
	var calls atomic.Int32
	var bodies []string
	client := newTestClient(func(config *Config) {
		config.HTTPClient = newHandlerHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(body))
			if calls.Add(1) == 1 {
				http.Error(w, "temporary", http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
	})

	req, err := http.NewRequest(http.MethodPost, "http://upstream.test/reservations", strings.NewReader(`{"sku":"SKU-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "reserve-1")
	response, err := client.Do(context.Background(), req, Policy{MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got, want := bodies, []string{`{"sku":"SKU-1"}`, `{"sku":"SKU-1"}`}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("bodies=%q, want %q", got, want)
	}
}

func TestClientClosesSourceBodyWhenUsingGetBody(t *testing.T) {
	sourceBody := &trackingBody{Reader: strings.NewReader(`{"sku":"SKU-1"}`)}
	client := newTestClient(func(config *Config) {
		config.HTTPClient = newHandlerHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	})
	req, err := http.NewRequest(http.MethodPost, "http://upstream.test/reservations", sourceBody)
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"sku":"SKU-1"}`)), nil
	}
	req.Header.Set("Idempotency-Key", "reserve-1")

	response, err := client.Do(context.Background(), req, Policy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if !sourceBody.closed {
		t.Fatal("source request body was not closed")
	}
}

func TestClientRetriesConnectionErrorsAndRetryableStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			client := newTestClient(func(config *Config) {
				config.HTTPClient = newHandlerHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) == 1 {
						w.WriteHeader(status)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}))
			})

			req, err := http.NewRequest(http.MethodGet, "http://upstream.test/resource", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(context.Background(), req, Policy{MaxAttempts: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if calls.Load() != 2 {
				t.Fatalf("calls=%d, want 2", calls.Load())
			}
		})
	}

	var calls atomic.Int32
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("connection reset")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
	client := newTestClient(func(config *Config) { config.HTTPClient = &http.Client{Transport: transport} })
	req, err := http.NewRequest(http.MethodGet, "http://upstream.test/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), req, Policy{MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls.Load() != 2 {
		t.Fatalf("calls=%d, want 2", calls.Load())
	}
}

func TestClientDrainsAndClosesRetryableResponses(t *testing.T) {
	firstBody := &trackingBody{Reader: strings.NewReader("discard me")}
	var calls atomic.Int32
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: firstBody, Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
	client := newTestClient(func(config *Config) { config.HTTPClient = &http.Client{Transport: transport} })
	req, err := http.NewRequest(http.MethodGet, "http://upstream.test/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), req, Policy{MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if !firstBody.closed || firstBody.read != len("discard me") {
		t.Fatalf("closed=%t read=%d, want closed=true read=%d", firstBody.closed, firstBody.read, len("discard me"))
	}
}

func TestClientHonorsTotalDeadlineDuringBackoff(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("connection reset")
	})
	client := newTestClient(func(config *Config) {
		config.HTTPClient = &http.Client{Transport: transport}
		config.TotalTimeout = 25 * time.Millisecond
		config.AttemptTimeout = 20 * time.Millisecond
		config.BaseBackoff = 100 * time.Millisecond
		config.MaxBackoff = 100 * time.Millisecond
	})
	req, err := http.NewRequest(http.MethodGet, "http://upstream.test/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = client.Do(context.Background(), req, Policy{MaxAttempts: 3})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do() error=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("Do() took %s, total deadline was 25ms", elapsed)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want 1", calls.Load())
	}
}

func TestClientPropagatesRequestAndTraceHeaders(t *testing.T) {
	missingHeader := ""
	client := newTestClient(func(config *Config) {
		config.HTTPClient = newHandlerHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, header := range []string{"X-Request-ID", "traceparent", "Idempotency-Key"} {
				if r.Header.Get(header) == "" {
					missingHeader = header
				}
			}
			w.WriteHeader(http.StatusNoContent)
		}))
	})

	req, err := http.NewRequest(http.MethodPost, "http://upstream.test/checkout", bytes.NewBufferString("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-ID", "request-7")
	req.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	req.Header.Set("Idempotency-Key", "checkout-7")
	response, err := client.Do(context.Background(), req, Policy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if missingHeader != "" {
		t.Fatalf("missing propagated %s header", missingHeader)
	}
}

func TestClientUsesRetryAfter(t *testing.T) {
	var calls atomic.Int32
	var slept time.Duration
	client := newTestClient(func(config *Config) {
		config.HTTPClient = newHandlerHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		config.Sleep = func(context.Context, time.Duration) error {
			slept = 2 * time.Second
			return nil
		}
	})
	req, err := http.NewRequest(http.MethodGet, "http://upstream.test/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), req, Policy{MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if slept != 2*time.Second {
		t.Fatalf("sleep=%s, want 2s", slept)
	}
}

func TestClientScopesCircuitBreakersPerUpstream(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(func(config *Config) {
		config.Breaker = BreakerConfig{FailureThreshold: 1, OpenFor: time.Second}
		config.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			statusCode := http.StatusNoContent
			if request.URL.Host == "unhealthy.test" {
				statusCode = http.StatusServiceUnavailable
			}
			return &http.Response{
				StatusCode: statusCode,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    request,
			}, nil
		})}
	})

	first, err := http.NewRequest(http.MethodGet, "http://unhealthy.test/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), first, Policy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(context.Background(), first, Policy{MaxAttempts: 1}); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("same-upstream Do() error=%v, want ErrCircuitOpen", err)
	}

	healthy, err := http.NewRequest(http.MethodGet, "http://healthy.test/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = client.Do(context.Background(), healthy, Policy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls.Load() != 2 {
		t.Fatalf("calls=%d, want 2", calls.Load())
	}
}

func TestClientKeepsSuccessfulAttemptContextUntilResponseBodyCloses(t *testing.T) {
	var attemptContext context.Context
	client := newTestClient(func(config *Config) {
		config.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			attemptContext = request.Context()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       &contextAwareBody{ctx: request.Context(), Reader: strings.NewReader("streamed response")},
				Request:    request,
			}, nil
		})}
	})
	req, err := http.NewRequest(http.MethodGet, "http://upstream.test/stream", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(context.Background(), req, Policy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-attemptContext.Done():
		t.Fatal("successful response attempt context was canceled before response body closed")
	default:
	}
	contents, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(contents), "streamed response"; got != want {
		t.Fatalf("response body=%q, want %q", got, want)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-attemptContext.Done():
	case <-time.After(time.Second):
		t.Fatal("response body close did not cancel successful attempt context")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

type handlerTransport struct{ handler http.Handler }

func newHandlerHTTPClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: handlerTransport{handler: handler}}
}

func (transport handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		defer request.Body.Close()
	}
	writer := &inMemoryResponseWriter{header: make(http.Header), statusCode: http.StatusOK}
	transport.handler.ServeHTTP(writer, request)
	return &http.Response{
		StatusCode: writer.statusCode,
		Header:     writer.header.Clone(),
		Body:       io.NopCloser(bytes.NewReader(writer.body.Bytes())),
		Request:    request,
	}, nil
}

type inMemoryResponseWriter struct {
	header     http.Header
	body       bytes.Buffer
	statusCode int
	wroteHead  bool
}

func (writer *inMemoryResponseWriter) Header() http.Header { return writer.header }

func (writer *inMemoryResponseWriter) WriteHeader(statusCode int) {
	if writer.wroteHead {
		return
	}
	writer.statusCode = statusCode
	writer.wroteHead = true
}

func (writer *inMemoryResponseWriter) Write(contents []byte) (int, error) {
	if !writer.wroteHead {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.body.Write(contents)
}

type trackingBody struct {
	io.Reader
	read   int
	closed bool
}

func (body *trackingBody) Read(bytes []byte) (int, error) {
	n, err := body.Reader.Read(bytes)
	body.read += n
	return n, err
}

func (body *trackingBody) Close() error {
	body.closed = true
	return nil
}

type contextAwareBody struct {
	ctx context.Context
	io.Reader
}

func (body *contextAwareBody) Read(bytes []byte) (int, error) {
	if err := body.ctx.Err(); err != nil {
		return 0, err
	}
	return body.Reader.Read(bytes)
}

func (body *contextAwareBody) Close() error { return nil }
