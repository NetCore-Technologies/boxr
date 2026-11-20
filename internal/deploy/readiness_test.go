package deploy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadinessProbeTCPConnectsToLoopback(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	probe := &ReadinessProbe{
		TCPAddress: listener.Addr().String(),
		Attempts:   1,
		TimeoutMS:  250,
	}
	if err := WaitForReadiness(context.Background(), probe); err != nil {
		t.Fatalf("WaitForReadiness(TCP): %v", err)
	}
}

func TestReadinessProbeHTTPRetriesUntilExpectedStatus(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) < 3 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	probe := &ReadinessProbe{
		HTTPURL:        server.URL + "/healthz",
		IntervalMS:     10,
		TimeoutMS:      250,
		Attempts:       4,
		ExpectedStatus: http.StatusNoContent,
	}
	if err := WaitForReadiness(context.Background(), probe); err != nil {
		t.Fatalf("WaitForReadiness(HTTP): %v", err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("HTTP request count = %d, want 3", got)
	}
}

func TestReadinessProbeDoesNotFollowRedirects(t *testing.T) {
	var destinationRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		destinationRequests.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusFound)
	}))
	defer redirect.Close()

	err := WaitForReadiness(context.Background(), &ReadinessProbe{
		HTTPURL:    redirect.URL,
		Attempts:   1,
		TimeoutMS:  250,
	})
	if err == nil {
		t.Fatal("redirect unexpectedly satisfied the HTTP readiness probe")
	}
	if got := destinationRequests.Load(); got != 0 {
		t.Fatalf("redirect destination received %d requests, want 0", got)
	}
}

func TestReadinessProbeHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitForReadiness(ctx, &ReadinessProbe{
		TCPAddress: "127.0.0.1:1",
		Attempts:   20,
		IntervalMS: 100,
	})
	if err != context.Canceled {
		t.Fatalf("WaitForReadiness error = %v, want context.Canceled", err)
	}
}

func TestReadinessProbeRejectsUnsafeAndUnboundedTargets(t *testing.T) {
	tests := map[string]ReadinessProbe{
		"missing target":                  {},
		"both target types":               {TCPAddress: "127.0.0.1:80", HTTPURL: "http://127.0.0.1/"},
		"external tcp host":                {TCPAddress: "example.com:80"},
		"invalid tcp port":                 {TCPAddress: "127.0.0.1:0"},
		"external http host":               {HTTPURL: "http://example.com/health"},
		"credentials in url":               {HTTPURL: "http://user:password@127.0.0.1/health"},
		"query in url":                     {HTTPURL: "http://127.0.0.1/health?token=secret"},
		"unsupported scheme":               {HTTPURL: "ftp://127.0.0.1/health"},
		"unbounded attempts":               {TCPAddress: "127.0.0.1:80", Attempts: maxReadinessAttempts + 1},
		"unbounded interval":               {TCPAddress: "127.0.0.1:80", IntervalMS: int(maxReadinessInterval/time.Millisecond) + 1},
		"unbounded timeout":                {TCPAddress: "127.0.0.1:80", TimeoutMS: int(maxReadinessTimeout/time.Millisecond) + 1},
		"invalid expected status":          {HTTPURL: "http://127.0.0.1/", ExpectedStatus: http.StatusBadRequest},
		"interval below minimum":           {TCPAddress: "127.0.0.1:80", IntervalMS: 1},
		"timeout below minimum":            {TCPAddress: "127.0.0.1:80", TimeoutMS: 1},
	}
	for name, probe := range tests {
		t.Run(name, func(t *testing.T) {
			if err := probe.Validate(); err == nil {
				t.Fatal("unsafe readiness probe unexpectedly validated")
			}
		})
	}
}

func TestReadinessProbeDefaultsAreBounded(t *testing.T) {
	probe := &ReadinessProbe{TCPAddress: "127.0.0.1:12345"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := WaitForReadiness(ctx, probe)
	if err == nil {
		t.Fatal("closed localhost port unexpectedly became ready")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("WaitForReadiness error = %v, want context.DeadlineExceeded", err)
	}
}
