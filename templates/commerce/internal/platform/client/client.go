// Package client provides bounded, resilient HTTP calls between commerce services.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	defaultTotalTimeout   = 10 * time.Second
	defaultAttemptTimeout = 3 * time.Second
	defaultBackoff        = 50 * time.Millisecond
	defaultMaxBackoff     = time.Second
	defaultMaxAttempts    = 3
)

// Config controls the transport, time limits, retries, and per-upstream
// breakers used by a Client. Jitter and Sleep are injectable for deterministic
// tests.
type Config struct {
	HTTPClient     *http.Client
	TotalTimeout   time.Duration
	AttemptTimeout time.Duration
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	MaxAttempts    int
	Jitter         func(time.Duration) time.Duration
	Sleep          func(context.Context, time.Duration) error
	Breaker        BreakerConfig
}

// Policy supplies request-specific retry limits and an optional shorter
// per-attempt deadline.
type Policy struct {
	MaxAttempts    int
	AttemptTimeout time.Duration
}

// Client performs resilient internal HTTP requests.
type Client struct {
	httpClient     *http.Client
	totalTimeout   time.Duration
	attemptTimeout time.Duration
	baseBackoff    time.Duration
	maxBackoff     time.Duration
	maxAttempts    int
	jitter         func(time.Duration) time.Duration
	sleep          func(context.Context, time.Duration) error
	breakerConfig  BreakerConfig
	breakers       sync.Map // map[string]*Breaker, keyed by scheme and authority
}

// New creates a client with bounded defaults suitable for internal calls.
func New(config Config) *Client {
	if config.HTTPClient == nil {
		config.HTTPClient = http.DefaultClient
	}
	if config.TotalTimeout <= 0 {
		config.TotalTimeout = defaultTotalTimeout
	}
	if config.AttemptTimeout <= 0 {
		config.AttemptTimeout = defaultAttemptTimeout
	}
	if config.BaseBackoff <= 0 {
		config.BaseBackoff = defaultBackoff
	}
	if config.MaxBackoff <= 0 {
		config.MaxBackoff = defaultMaxBackoff
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = defaultMaxAttempts
	}
	if config.Jitter == nil {
		config.Jitter = defaultJitter
	}
	if config.Sleep == nil {
		config.Sleep = sleep
	}
	return &Client{
		httpClient:     config.HTTPClient,
		totalTimeout:   config.TotalTimeout,
		attemptTimeout: config.AttemptTimeout,
		baseBackoff:    config.BaseBackoff,
		maxBackoff:     config.MaxBackoff,
		maxAttempts:    config.MaxAttempts,
		jitter:         config.Jitter,
		sleep:          config.Sleep,
		breakerConfig:  config.Breaker,
	}
}

// Do sends request with a total deadline, a per-attempt timeout, bounded retry,
// and a circuit breaker scoped to the request's upstream.
func (client *Client) Do(ctx context.Context, request *http.Request, policy Policy) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("client request is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	totalContext, cancelTotal := context.WithTimeout(ctx, client.totalTimeout)
	returnedResponse := false
	defer func() {
		if !returnedResponse {
			cancelTotal()
		}
	}()

	body, err := replayableBody(request)
	if err != nil {
		return nil, err
	}
	attempts := client.attempts(policy)
	retryAllowed := isRetryEligible(request)
	breaker := client.breakerFor(request)

	for attempt := 1; attempt <= attempts; attempt++ {
		if err := totalContext.Err(); err != nil {
			return nil, err
		}
		if err := breaker.Allow(); err != nil {
			return nil, err
		}

		attemptContext, cancelAttempt := context.WithTimeout(totalContext, client.attemptTimeoutFor(policy))
		attemptRequest, err := requestForAttempt(request, attemptContext, body)
		if err != nil {
			cancelAttempt()
			return nil, err
		}
		response, err := client.httpClient.Do(attemptRequest)
		if err == nil && attemptContext.Err() != nil {
			err = attemptContext.Err()
		}
		if err != nil {
			drainAndClose(response)
			cancelAttempt()
			breaker.Record(false)
			if !retryAllowed || attempt == attempts {
				if totalContext.Err() != nil {
					return nil, totalContext.Err()
				}
				return nil, err
			}
			if err := client.sleep(totalContext, client.retryDelay(attempt)); err != nil {
				return nil, err
			}
			continue
		}

		failed := isRetryableResponse(response)
		breaker.Record(!failed)
		if !failed || !retryAllowed || attempt == attempts {
			wrapResponseBody(response, cancelAttempt, cancelTotal)
			returnedResponse = true
			return response, nil
		}

		delay := retryAfter(response.Header.Get("Retry-After"), time.Now())
		drainAndClose(response)
		cancelAttempt()
		if delay <= 0 {
			delay = client.retryDelay(attempt)
		}
		if err := client.sleep(totalContext, delay); err != nil {
			return nil, err
		}
	}
	return nil, context.Canceled // unreachable, retained for compiler completeness
}

func (client *Client) retryDelay(attempt int) time.Duration {
	delay := client.jitter(client.backoff(attempt))
	if delay < 0 {
		return 0
	}
	if delay > client.maxBackoff {
		return client.maxBackoff
	}
	return delay
}

func wrapResponseBody(response *http.Response, cancelAttempt, cancelTotal context.CancelFunc) {
	if response.Body == nil {
		response.Body = http.NoBody
	}
	response.Body = &cancellingBody{
		ReadCloser: response.Body,
		cancel: func() {
			cancelAttempt()
			cancelTotal()
		},
	}
}

type cancellingBody struct {
	io.ReadCloser
	once   sync.Once
	cancel context.CancelFunc
}

func (body *cancellingBody) Close() error {
	body.once.Do(body.cancel)
	return body.ReadCloser.Close()
}

func (client *Client) attempts(policy Policy) int {
	if policy.MaxAttempts > 0 {
		return policy.MaxAttempts
	}
	return client.maxAttempts
}

func (client *Client) attemptTimeoutFor(policy Policy) time.Duration {
	if policy.AttemptTimeout > 0 {
		return policy.AttemptTimeout
	}
	return client.attemptTimeout
}

func (client *Client) breakerFor(request *http.Request) *Breaker {
	key := request.URL.Scheme + "://" + request.URL.Host
	created := NewBreaker(client.breakerConfig)
	actual, _ := client.breakers.LoadOrStore(key, created)
	return actual.(*Breaker)
}

func (client *Client) backoff(attempt int) time.Duration {
	delay := client.baseBackoff
	for retry := 1; retry < attempt && delay < client.maxBackoff; retry++ {
		if delay > client.maxBackoff/2 {
			return client.maxBackoff
		}
		delay *= 2
	}
	if delay > client.maxBackoff {
		return client.maxBackoff
	}
	return delay
}

func isRetryEligible(request *http.Request) bool {
	return request.Method == http.MethodGet || request.Method == http.MethodHead || request.Header.Get("Idempotency-Key") != ""
}

func isRetryableResponse(response *http.Response) bool {
	if response == nil {
		return false
	}
	switch response.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func replayableBody(request *http.Request) (func() (io.ReadCloser, error), error) {
	if request.Body == nil || request.Body == http.NoBody {
		return func() (io.ReadCloser, error) { return nil, nil }, nil
	}
	if request.GetBody != nil {
		if err := request.Body.Close(); err != nil {
			return nil, err
		}
		return request.GetBody, nil
	}
	contents, err := io.ReadAll(request.Body)
	closeErr := request.Body.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(contents)), nil
	}, nil
}

func requestForAttempt(request *http.Request, ctx context.Context, body func() (io.ReadCloser, error)) (*http.Request, error) {
	attempt := request.Clone(ctx)
	attemptBody, err := body()
	if err != nil {
		return nil, err
	}
	attempt.Body = attemptBody
	attempt.GetBody = body
	return attempt, nil
}

func drainAndClose(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := when.Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func defaultJitter(delay time.Duration) time.Duration {
	if delay <= 1 {
		return delay
	}
	var random [1]byte
	if _, err := rand.Read(random[:]); err != nil {
		return delay
	}
	return delay/2 + time.Duration(random[0])*delay/(2*255)
}
