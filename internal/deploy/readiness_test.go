package deploy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
		HTTPURL:   redirect.URL,
		Attempts:  1,
		TimeoutMS: 250,
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
		"whitespace TCP with HTTP target": {TCPAddress: " ", HTTPURL: "http://127.0.0.1/"},
		"whitespace HTTP with TCP target": {TCPAddress: "127.0.0.1:80", HTTPURL: " "},
		"both target types":               {TCPAddress: "127.0.0.1:80", HTTPURL: "http://127.0.0.1/"},
		"external tcp host":               {TCPAddress: "example.com:80"},
		"localhost hostname":              {TCPAddress: "localhost:80"},
		"TCP probe with HTTP status":      {TCPAddress: "127.0.0.1:80", ExpectedStatus: http.StatusOK},
		"invalid tcp port":                {TCPAddress: "127.0.0.1:0"},
		"TCP port with plus sign":         {TCPAddress: "127.0.0.1:+80"},
		"external http host":              {HTTPURL: "http://example.com/health"},
		"credentials in url":              {HTTPURL: "http://user:password@127.0.0.1/health"},
		"query in url":                    {HTTPURL: "http://127.0.0.1/health?token=secret"},
		"empty query delimiter":           {HTTPURL: "http://127.0.0.1/health?"},
		"empty fragment delimiter":        {HTTPURL: "http://127.0.0.1/health#"},
		"unsupported scheme":              {HTTPURL: "ftp://127.0.0.1/health"},
		"unbounded attempts":              {TCPAddress: "127.0.0.1:80", Attempts: maxReadinessAttempts + 1},
		"unbounded interval":              {TCPAddress: "127.0.0.1:80", IntervalMS: int(maxReadinessInterval/time.Millisecond) + 1},
		"unbounded timeout":               {TCPAddress: "127.0.0.1:80", TimeoutMS: int(maxReadinessTimeout/time.Millisecond) + 1},
		"invalid expected status":         {HTTPURL: "http://127.0.0.1/", ExpectedStatus: http.StatusBadRequest},
		"interval below minimum":          {TCPAddress: "127.0.0.1:80", IntervalMS: 1},
		"timeout below minimum":           {TCPAddress: "127.0.0.1:80", TimeoutMS: 1},
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

func TestHealthcheckFailsAfterConsecutiveUnhealthyChecks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := MonitorHealth(context.Background(), &Healthcheck{
		Probe:      ReadinessProbe{HTTPURL: server.URL + "/healthz", TimeoutMS: 100},
		IntervalMS: 10,
		Retries:    2,
	})
	if err == nil || !strings.Contains(err.Error(), "2 consecutive failure(s)") {
		t.Fatalf("MonitorHealth() error = %v, want two consecutive healthcheck failures", err)
	}
}

func TestHealthcheckSuccessResetsConsecutiveFailureCount(t *testing.T) {
	var requests atomic.Int32
	reachedFour := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempt := requests.Add(1)
		reachedFour <- struct{}{}
		if attempt == 1 || attempt == 3 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- MonitorHealth(ctx, &Healthcheck{
			Probe:      ReadinessProbe{HTTPURL: server.URL + "/healthz", TimeoutMS: 100},
			IntervalMS: 10,
			Retries:    2,
		})
	}()
	for count := 0; count < 4; count++ {
		select {
		case <-reachedFour:
		case err := <-result:
			cancel()
			t.Fatalf("healthcheck stopped before four probes; success should reset failures: %v", err)
		case <-time.After(time.Second):
			cancel()
			t.Fatal("healthcheck did not perform four probes")
		}
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("MonitorHealth() after cancellation = %v, want context.Canceled", err)
	}
}

func TestHealthcheckStartPeriodDefersFirstProbe(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- MonitorHealth(ctx, &Healthcheck{
			Probe:         ReadinessProbe{HTTPURL: server.URL},
			IntervalMS:    10,
			Retries:       2,
			StartPeriodMS: 250,
		})
	}()
	time.Sleep(25 * time.Millisecond)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("MonitorHealth() after start-period cancellation = %v, want context.Canceled", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("healthcheck made %d requests during its start period, want 0", got)
	}
}

func TestHealthcheckValidatesItsOwnTimingAndRetryBudget(t *testing.T) {
	base := Healthcheck{Probe: ReadinessProbe{TCPAddress: "127.0.0.1:8080"}}
	tests := map[string]Healthcheck{
		"interval below minimum": {Probe: base.Probe, IntervalMS: 1},
		"interval above maximum": {Probe: base.Probe, IntervalMS: 60001},
		"negative retries":       {Probe: base.Probe, Retries: -1},
		"too many retries":       {Probe: base.Probe, Retries: 11},
		"negative start period":  {Probe: base.Probe, StartPeriodMS: -1},
		"long start period":      {Probe: base.Probe, StartPeriodMS: 600001},
		"probe has multiple attempts": {
			Probe: ReadinessProbe{TCPAddress: "127.0.0.1:8080", Attempts: 2},
		},
		"probe has its own interval": {
			Probe: ReadinessProbe{TCPAddress: "127.0.0.1:8080", IntervalMS: 100},
		},
	}
	for name, health := range tests {
		t.Run(name, func(t *testing.T) {
			if err := health.Validate(); err == nil {
				t.Fatal("invalid healthcheck unexpectedly validated")
			}
		})
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid healthcheck defaults: %v", err)
	}
}
