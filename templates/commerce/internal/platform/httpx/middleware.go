package httpx

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

type requestIDKey struct{}

// RequestID returns the request identifier installed by the server middleware.
func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func serviceInstance(instance string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if instance != "" {
			w.Header().Set("X-Service-Instance", instance)
		}
		next.ServeHTTP(w, r)
	})
}

func recoverPanic(logger *slog.Logger, service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("panic while serving HTTP request", "service", service, "panic", recovered, "request_id", RequestID(r.Context()))
				if state, ok := w.(interface{ Committed() bool }); ok && state.Committed() {
					panic(http.ErrAbortHandler)
				}
				WriteProblem(w, Problem{Status: http.StatusInternalServerError, Code: "internal_error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func limitBody(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func accessLog(logger *slog.Logger, service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		logger.Info("http request", "service", service, "method", r.Method, "path", r.URL.Path, "status", recorder.status,
			"bytes", recorder.bytes, "duration", time.Since(started), "request_id", RequestID(r.Context()))
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status    int
	bytes     int
	committed bool
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.committed {
		return
	}
	w.status = status
	w.committed = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(body []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += n
	return n, err
}

// Committed reports whether response headers have been sent.
func (w *responseRecorder) Committed() bool { return w.committed }

// Unwrap preserves optional ResponseWriter interfaces for ResponseController.
func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// FlushError marks the response committed before delegating optional flush support.
func (w *responseRecorder) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err == nil && !w.committed {
		w.status = http.StatusOK
		w.committed = true
	}
	return err
}

// Flush delegates direct legacy flushing while retaining response state.
func (w *responseRecorder) Flush() { _ = w.FlushError() }

// Hijack delegates connection hijacking when the underlying writer supports it.
func (w *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	connection, readerWriter, err := hijacker.Hijack()
	if err == nil && !w.committed {
		w.status = http.StatusOK
		w.committed = true
	}
	return connection, readerWriter, err
}

// Push delegates HTTP/2 server push when the underlying writer supports it.
func (w *responseRecorder) Push(target string, options *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

// ReadFrom delegates efficient response copying and records the emitted bytes.
func (w *responseRecorder) ReadFrom(source io.Reader) (int64, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := readerFrom.ReadFrom(source)
		w.bytes += int(n)
		return n, err
	}
	n, err := io.Copy(writerOnly{w.ResponseWriter}, source)
	w.bytes += int(n)
	return n, err
}

type writerOnly struct{ io.Writer }

func newRequestID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return time.Now().UTC().Format("20060102T150405.000000000")
}
