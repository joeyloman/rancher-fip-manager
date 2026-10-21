package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/joeyloman/rancher-fip-api-server/internal/handlers"
	"github.com/sirupsen/logrus"
)

// The probe endpoints must answer on their own plain-HTTP port: the old
// tcpSocket probes dialed the TLS listener and logged a "TLS handshake
// error" line per tick. /healthz is unconditional (liveness from process
// start); /readyz is gated on apiListenerReady, so it stays 503 until the
// API listener is actually bound — readiness keeps meaning "the service
// port answers", which the old tcpSocket probe guaranteed.
func TestServeHealthProbe_ReadyzGatedOnListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // serveHealthProbe binds the address itself

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveHealthProbe(ctx, logrus.New(), addr)

	client := &http.Client{Timeout: 3 * time.Second}
	get := func(path string) (*http.Response, error) {
		deadline := time.Now().Add(3 * time.Second)
		for {
			resp, err := client.Get("http://" + addr + path)
			if err == nil || time.Now().After(deadline) {
				return resp, err
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// /healthz answers immediately; /readyz is 503 while nothing serves
	// on the API port.
	resp, err := get("/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %s", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: status %d, want 200", resp.StatusCode)
	}

	resp, err = get("/readyz")
	if err != nil {
		t.Fatalf("GET /readyz before listener: %s", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz before listener: status %d, want 503", resp.StatusCode)
	}
	if string(body) != "api listener not ready" {
		t.Fatalf("GET /readyz before listener: body %q", body)
	}

	// The API listener binds: the flag flips exactly at bind time.
	apiListenerReady.Store(true)
	resp, err = get("/readyz")
	if err != nil {
		t.Fatalf("GET /readyz after listener: %s", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz after listener: status %d, want 200", resp.StatusCode)
	}
	if string(body) != "ok" {
		t.Fatalf("GET /readyz after listener: body %q, want \"ok\"", body)
	}

	// Shutdown path: cancel must stop the listener without leaking it.
	cancel()
	shutdownDeadline := time.Now().Add(3 * time.Second)
	for {
		c, derr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if derr != nil {
			break
		}
		c.Close()
		if time.Now().After(shutdownDeadline) {
			t.Fatalf("listener still accepting after cancel")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// shutdownGracePeriod is derived from the handlers package so the two cannot
// drift: if a handler budget grows, the drain window grows with it. A shorter
// grace (the former hard-coded 5s) made server.Shutdown hit its deadline and
// log.Fatalf while an allocation handler was still mid-write.
func TestShutdownGracePeriodCoversWorstCaseHandler(t *testing.T) {
	if got, want := shutdownGracePeriod, handlers.MaxRequestDuration(); got <= want {
		t.Fatalf("shutdownGracePeriod = %s, must exceed worst-case handler %s", got, want)
	}
	if shutdownGracePeriod <= 30*time.Second+10*time.Second {
		t.Fatalf("shutdownGracePeriod = %s, must cover the 30s allocation wait plus 10s cleanup", shutdownGracePeriod)
	}
}

// TestShutdownAPIWaitsForInFlightHandler pins the drain behaviour: a request
// that started before the signal finishes and its response is delivered,
// instead of the process exiting mid-write.
func TestShutdownAPIWaitsForInFlightHandler(t *testing.T) {
	started := make(chan struct{})
	srv, ln := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte("ok"))
	}))

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		done <- result{body: string(b), err: err}
	}()

	<-started
	begin := time.Now()
	shutdownAPI(srv, discardLogger(), 2*time.Second)
	if elapsed := time.Since(begin); elapsed < 200*time.Millisecond {
		t.Fatalf("shutdownAPI returned after %s, before the in-flight handler finished", elapsed)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("in-flight request failed during drain: %s", res.err)
		}
		if res.body != "ok" {
			t.Fatalf("in-flight request body = %q, want %q", res.body, "ok")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request never completed after shutdownAPI returned")
	}
}

// TestShutdownAPIForceClosesStuckHandler covers the residual: a handler that
// outlives the grace is force-closed, and shutdownAPI still returns instead of
// blocking (or killing the process) forever.
func TestShutdownAPIForceClosesStuckHandler(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	srv, ln := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))

	done := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			done <- err
			return
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		done <- err
	}()

	<-started
	begin := time.Now()
	shutdownAPI(srv, discardLogger(), 100*time.Millisecond)
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Fatalf("shutdownAPI blocked for %s with a stuck handler", elapsed)
	}

	select {
	case <-done:
		// Either a connection error or a truncated read is expected: the
		// point is that the client is released, not hung.
	case <-time.After(2 * time.Second):
		t.Fatal("client stayed connected after the grace expired")
	}
}

func startTestServer(t *testing.T, h http.Handler) (*http.Server, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}
	srv := &http.Server{Handler: h}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			t.Logf("serve: %s", err)
		}
	}()
	return srv, ln
}

func discardLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}
