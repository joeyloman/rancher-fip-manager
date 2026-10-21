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

// Regression for the leader-loss drain: OnStoppedLeading must
// cancel the running controller and give it a bounded grace period to finish
// its teardown instead of killing the process mid-drain.
func TestDrainRun_NeverStartedReturnsImmediately(t *testing.T) {
	runStart := make(chan context.CancelFunc, 1)
	runDone := make(chan struct{})

	done := make(chan bool, 1)
	go func() { done <- drainRun(runStart, runDone, 5*time.Second) }()

	select {
	case drained := <-done:
		if !drained {
			t.Fatal("drainRun reported timeout, want immediate success")
		}
	case <-time.After(time.Second):
		t.Fatal("drainRun blocked although leadership was never acquired")
	}
}

func TestDrainRun_CancelsAndWaitsForRun(t *testing.T) {
	runStart := make(chan context.CancelFunc, 1)
	runDone := make(chan struct{})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Simulate run(): returns (closing runDone) only after its context is
	// cancelled - i.e. the drain must actively cancel it.
	go func() {
		<-runCtx.Done()
		close(runDone)
	}()
	runStart <- cancel

	start := time.Now()
	drained := drainRun(runStart, runDone, 5*time.Second)
	if !drained {
		t.Fatal("drainRun reported timeout, want success")
	}
	if runCtx.Err() == nil {
		t.Fatal("drainRun did not cancel the run context")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("drain took %s after run returned, want prompt return", elapsed)
	}
}

func TestDrainRun_TimesOutWhenRunHangs(t *testing.T) {
	runStart := make(chan context.CancelFunc, 1)
	runDone := make(chan struct{}) // never closed: run() hangs
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStart <- cancel

	start := time.Now()
	if drainRun(runStart, runDone, 50*time.Millisecond) {
		t.Fatal("drainRun reported success, want timeout")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("drainRun returned after %s, want >= grace period", elapsed)
	}
}
