package middleware

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBodyLimitMiddlewareRejectsLargeContentLength(t *testing.T) {
	nextCalled := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true })

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bytes.Repeat([]byte("x"), 2048)))
	require.EqualValues(t, 2048, req.ContentLength)
	w := httptest.NewRecorder()

	BodyLimitMiddleware(1024)(next).ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.False(t, nextCalled, "oversized requests must not reach the handler")
}

func TestBodyLimitMiddlewareCapsUnboundedStream(t *testing.T) {
	var readErr error
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Body = io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), 4096)))
	req.ContentLength = -1 // simulate chunked encoding (unknown length)
	w := httptest.NewRecorder()

	BodyLimitMiddleware(1024)(next).ServeHTTP(w, req)

	require.Error(t, readErr, "reading past the limit must fail")
	var maxBytesErr *http.MaxBytesError
	assert.True(t, stderrors.As(readErr, &maxBytesErr), "reader must see http.MaxBytesError, got %v", readErr)
}

func TestBodyLimitMiddlewareAllowsSmallBody(t *testing.T) {
	var body []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"project":"p"}`)))
	w := httptest.NewRecorder()

	BodyLimitMiddleware(1024)(next).ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"project":"p"}`, string(body))
}

// freezeLimiterClock makes a limiterStore's eviction clock controllable; the
// token buckets themselves use real time, so the tests pick a near-zero refill
// rate and rely only on each bucket's initial burst.
func freezeLimiterClock(store *limiterStore, at time.Time) {
	store.now = func() time.Time { return at }
}

func serveRateLimited(handler http.Handler, remoteAddr, clientID string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/fip/request", nil)
	req.RemoteAddr = remoteAddr
	if clientID != "" {
		req = req.WithContext(context.WithValue(req.Context(), ClientIDContextKey, clientID))
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Code
}

func TestRateLimitMiddlewareIsolatesAuthenticatedClients(t *testing.T) {
	store := newLimiterStore(0.001, 1)
	freezeLimiterClock(store, time.Unix(1700000000, 0))
	handler := rateLimitHandler(store, ClientIDRateLimitKey)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	// Same peer address for both clients: the bucket must follow the client
	// ID, not the connection.
	assert.Equal(t, http.StatusOK, serveRateLimited(handler, "192.0.2.10:4444", "alpha"))
	assert.Equal(t, http.StatusTooManyRequests, serveRateLimited(handler, "192.0.2.10:4444", "alpha"),
		"alpha's burst must be exhausted")
	assert.Equal(t, http.StatusOK, serveRateLimited(handler, "192.0.2.10:4444", "beta"),
		"a noisy client must not consume another client's budget")
	assert.Equal(t, http.StatusTooManyRequests, serveRateLimited(handler, "192.0.2.10:4444", "beta"))
}

func TestRateLimitMiddlewareBucketsByPeerAddressWithoutClientID(t *testing.T) {
	store := newLimiterStore(0.001, 1)
	freezeLimiterClock(store, time.Unix(1700000000, 0))
	handler := rateLimitHandler(store, ClientIDRateLimitKey)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	assert.Equal(t, http.StatusOK, serveRateLimited(handler, "192.0.2.1:1000", ""))
	assert.Equal(t, http.StatusTooManyRequests, serveRateLimited(handler, "192.0.2.1:2000", ""),
		"a different source port is the same peer")
	assert.Equal(t, http.StatusOK, serveRateLimited(handler, "192.0.2.2:1000", ""),
		"a different peer gets its own bucket")

	// A spoofed X-Forwarded-For must not mint a fresh bucket for an exhausted
	// peer: the key is the TCP address, never a caller-supplied header.
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/token", nil)
	req.RemoteAddr = "192.0.2.1:3000"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// TestRateLimitMiddlewareGlobalCeilingBoundsClientIDChurn pins the backstop
// for the caller-chosen client ID: minting a fresh client ID per request must
// not buy unlimited throughput once the aggregate ceiling is reached.
func TestRateLimitMiddlewareGlobalCeilingBoundsClientIDChurn(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	ceiling := rateLimitHandler(newLimiterStore(0.001, 3), GlobalRateLimitKey)(ok)
	// Per-client bucket is wide enough that only the ceiling can deny.
	handler := rateLimitHandler(newLimiterStore(0.001, 100), ClientIDRateLimitKey)(ceiling)

	allowed, denied := 0, 0
	for i := range 6 {
		switch code := serveRateLimited(handler, "192.0.2.50:1000", fmt.Sprintf("churn-%d", i)); code {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
			denied++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}

	assert.Equal(t, 3, allowed, "the aggregate ceiling must cap a client-ID churn")
	assert.Equal(t, 3, denied)
}

func TestLimiterStoreEvictsIdleBuckets(t *testing.T) {
	store := newLimiterStore(10, 1)
	now := time.Unix(1700000000, 0)
	store.now = func() time.Time { return now }

	require.True(t, store.allow("client:alpha"))
	require.False(t, store.allow("client:alpha"), "alpha's single token is spent")
	require.Len(t, store.entries, 1)

	now = now.Add(limiterIdleTTL + limiterSweepInterval)
	require.True(t, store.allow("client:beta"))

	assert.NotContains(t, store.entries, "client:alpha", "an idle bucket must be forgotten")
	assert.Contains(t, store.entries, "client:beta")
	assert.True(t, store.allow("client:alpha"), "an evicted client gets a fresh bucket")
}

// TestRateLimitMiddlewareBoundsTokenRouteIPChurn exercises the address-churn
// path through the middleware the token route actually uses: the bucket map
// must stay capped no matter how many distinct source addresses arrive.
func TestRateLimitMiddlewareBoundsTokenRouteIPChurn(t *testing.T) {
	store := newLimiterStore(0.001, 1)
	freezeLimiterClock(store, time.Unix(1700000000, 0))
	handler := rateLimitHandler(store, RemoteIPRateLimitKey)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	for i := range limiterMaxEntries + 500 {
		serveRateLimited(handler, fmt.Sprintf("198.51.%d.%d:1000", i/256, i%256), "")
	}

	assert.LessOrEqual(t, len(store.entries), limiterMaxEntries)
}
