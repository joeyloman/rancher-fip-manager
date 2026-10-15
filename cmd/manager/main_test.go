package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// Regression for the rollout deadlock: the probe endpoints must answer
// before leader election, or a standby candidate never goes Ready, the
// ReplicaSet never terminates the old leader (which keeps renewing the
// lease), and liveness kills the candidate in a loop.
func TestServeHealthProbe_BindsBeforeLeadership(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // serveHealthProbe binds the address itself

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveHealthProbe(ctx, addr)

	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(3 * time.Second)
	for _, path := range []string{"/healthz", "/readyz"} {
		var resp *http.Response
		for {
			resp, err = client.Get("http://" + addr + path)
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("GET %s before leadership: %s", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200", path, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "ok" {
			t.Fatalf("GET %s: body %q, want \"ok\"", path, body)
		}
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
			t.Fatal("health probe server still listening after ctx cancel")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
