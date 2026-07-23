package serviceclient

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestGetDecodesJSON(t *testing.T) {
	client := New("http://customer:18081/")
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "http://customer:18081/api/v1/customers/C%201" {
			t.Fatalf("URL=%s", req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(`{"name":"测试客户"}`)),
			Header:     make(http.Header),
		}, nil
	})
	var got struct {
		Name string `json:"name"`
	}
	if err := client.Get(context.Background(), "/api/v1/customers/"+EscapePath("C 1"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "测试客户" {
		t.Fatalf("name=%q", got.Name)
	}
}

func TestGetRejectsNon2xx(t *testing.T) {
	client := New("http://customer:18081")
	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Body:       io.NopCloser(strings.NewReader(`{"error":"not ready"}`)),
			Header:     make(http.Header),
		}, nil
	})
	var out any
	err := client.Get(context.Background(), "/healthz", &out)
	if err == nil || !strings.Contains(err.Error(), "503 Service Unavailable") {
		t.Fatalf("err=%v", err)
	}
}
