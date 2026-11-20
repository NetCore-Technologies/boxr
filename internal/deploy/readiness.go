// Package deploy provides validation and lifecycle helpers for Boxr deployments.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultReadinessInterval = 250 * time.Millisecond
	defaultReadinessTimeout  = time.Second
	defaultReadinessAttempts = 30
	maxReadinessInterval     = 60 * time.Second
	maxReadinessTimeout      = 10 * time.Second
	maxReadinessAttempts     = 120
)

// ReadinessProbe gates dependent service startup on an application-level check.
// Targets are intentionally restricted to loopback because Boxr currently has
// host networking and isolated-network modes, but no bridge or port publishing.
type ReadinessProbe struct {
	TCPAddress     string `json:"tcpAddress,omitempty"`
	HTTPURL        string `json:"httpUrl,omitempty"`
	IntervalMS     int    `json:"intervalMs,omitempty"`
	TimeoutMS      int    `json:"timeoutMs,omitempty"`
	Attempts       int    `json:"attempts,omitempty"`
	ExpectedStatus int    `json:"expectedStatus,omitempty"`
}

// Type returns the configured probe protocol without disclosing its target.
func (p ReadinessProbe) Type() string {
	if p.TCPAddress != "" {
		return "tcp"
	}
	if p.HTTPURL != "" {
		return "http"
	}
	return ""
}

// Validate ensures the probe has one safe loopback target and bounded timing.
func (p ReadinessProbe) Validate() error {
	tcpAddress := strings.TrimSpace(p.TCPAddress)
	httpURL := strings.TrimSpace(p.HTTPURL)
	if (tcpAddress == "") == (httpURL == "") {
		return fmt.Errorf("exactly one of tcpAddress or httpUrl must be set")
	}
	if tcpAddress != "" {
		if tcpAddress != p.TCPAddress {
			return fmt.Errorf("tcpAddress must not contain surrounding whitespace")
		}
		host, portText, err := net.SplitHostPort(tcpAddress)
		if err != nil {
			return fmt.Errorf("tcpAddress must be a loopback host and explicit port")
		}
		if err := validateLoopbackHost(host); err != nil {
			return fmt.Errorf("tcpAddress: %w", err)
		}
		if err := validatePort(portText); err != nil {
			return fmt.Errorf("tcpAddress: %w", err)
		}
	}
	if httpURL != "" {
		if httpURL != p.HTTPURL {
			return fmt.Errorf("httpUrl must not contain surrounding whitespace")
		}
		parsed, err := url.ParseRequestURI(httpURL)
		if err != nil || parsed == nil {
			return fmt.Errorf("httpUrl must be a valid absolute HTTP or HTTPS URL")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("httpUrl scheme must be http or https")
		}
		if parsed.Opaque != "" || parsed.User != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("httpUrl must not contain credentials, a query, or a fragment")
		}
		if err := validateLoopbackHost(parsed.Hostname()); err != nil {
			return fmt.Errorf("httpUrl: %w", err)
		}
		if parsed.Port() != "" {
			if err := validatePort(parsed.Port()); err != nil {
				return fmt.Errorf("httpUrl: %w", err)
			}
		}
	}
	if p.IntervalMS < 0 || p.IntervalMS > int(maxReadinessInterval/time.Millisecond) ||
		(p.IntervalMS > 0 && p.IntervalMS < 10) {
		return fmt.Errorf("intervalMs must be 0 (default) or between 10 and %d", int(maxReadinessInterval/time.Millisecond))
	}
	if p.TimeoutMS < 0 || p.TimeoutMS > int(maxReadinessTimeout/time.Millisecond) ||
		(p.TimeoutMS > 0 && p.TimeoutMS < 10) {
		return fmt.Errorf("timeoutMs must be 0 (default) or between 10 and %d", int(maxReadinessTimeout/time.Millisecond))
	}
	if p.Attempts < 0 || p.Attempts > maxReadinessAttempts {
		return fmt.Errorf("attempts must be 0 (default) or between 1 and %d", maxReadinessAttempts)
	}
	if p.ExpectedStatus != 0 && (p.ExpectedStatus < 200 || p.ExpectedStatus > 399) {
		return fmt.Errorf("expectedStatus must be 0 (default) or an HTTP status from 200 to 399")
	}
	return nil
}

func validateLoopbackHost(host string) error {
	// Accept numeric loopback addresses only. Resolving arbitrary hostnames,
	// including localhost aliases, would make the target depend on host DNS
	// or hosts-file configuration and weaken the loopback-only guarantee.
	address := net.ParseIP(host)
	if address == nil || !address.IsLoopback() {
		return fmt.Errorf("only a loopback IP address is allowed")
	}
	return nil
}

func validatePort(value string) error {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port must be an integer between 1 and 65535")
	}
	return nil
}

// WaitForReadiness retries a validated TCP or HTTP probe until it succeeds,
// the attempt budget is exhausted, or the caller cancels the operation.
func WaitForReadiness(ctx context.Context, probe *ReadinessProbe) error {
	if probe == nil {
		return nil
	}
	if err := probe.Validate(); err != nil {
		return err
	}
	interval := defaultReadinessInterval
	if probe.IntervalMS > 0 {
		interval = time.Duration(probe.IntervalMS) * time.Millisecond
	}
	timeout := defaultReadinessTimeout
	if probe.TimeoutMS > 0 {
		timeout = time.Duration(probe.TimeoutMS) * time.Millisecond
	}
	attempts := probe.Attempts
	if attempts == 0 {
		attempts = defaultReadinessAttempts
	}
	expectedStatus := probe.ExpectedStatus
	if expectedStatus == 0 {
		expectedStatus = http.StatusOK
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = checkReadinessOnce(ctx, probe, timeout, expectedStatus)
		if lastErr == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt == attempts {
			break
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("%s readiness probe failed after %d attempt(s): %w", probe.Type(), attempts, lastErr)
}

func checkReadinessOnce(parent context.Context, probe *ReadinessProbe, timeout time.Duration, expectedStatus int) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if probe.TCPAddress != "" {
		dialer := net.Dialer{Timeout: timeout}
		connection, err := dialer.DialContext(ctx, "tcp", probe.TCPAddress)
		if err != nil {
			return errors.New("TCP connection failed")
		}
		_ = connection.Close()
		return nil
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probe.HTTPURL, nil)
	if err != nil {
		return errors.New("HTTP request could not be created")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: timeout}).DialContext}
	defer transport.CloseIdleConnections()
	client := http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("HTTP request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		return fmt.Errorf("HTTP probe returned status %d, expected %d", response.StatusCode, expectedStatus)
	}
	return nil
}
