package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

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
