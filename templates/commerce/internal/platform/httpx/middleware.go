package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
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
		buffer := &responseBuffer{header: make(http.Header), status: http.StatusOK}
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("panic while serving HTTP request", "service", service, "panic", recovered, "request_id", RequestID(r.Context()))
				WriteProblem(w, Problem{Status: http.StatusInternalServerError, Code: "internal_error"})
				return
			}
			buffer.flush(w)
		}()
		next.ServeHTTP(buffer, r)
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
	status int
	bytes  int
}

type responseBuffer struct {
	header      http.Header
	status      int
	body        []byte
	wroteHeader bool
}

func (w *responseBuffer) Header() http.Header { return w.header }

func (w *responseBuffer) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *responseBuffer) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.body = append(w.body, body...)
	return len(body), nil
}

func (w *responseBuffer) flush(destination http.ResponseWriter) {
	for key, values := range w.header {
		destination.Header()[key] = append([]string(nil), values...)
	}
	destination.WriteHeader(w.status)
	_, _ = destination.Write(w.body)
}

func (w *responseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(body []byte) (int, error) {
	n, err := w.ResponseWriter.Write(body)
	w.bytes += n
	return n, err
}

func newRequestID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return time.Now().UTC().Format("20060102T150405.000000000")
}
