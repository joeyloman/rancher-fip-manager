// Package middleware provides HTTP middleware functions for the API.
package middleware

import (
	"context"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"bytes"
	"encoding/json"
	stderrors "errors"
	"io"

	"golang.org/x/time/rate"

	"github.com/joeyloman/rancher-fip-api-server/internal/auth"
	"github.com/joeyloman/rancher-fip-api-server/internal/errors"
	"github.com/joeyloman/rancher-fip-api-server/pkg/types"
)

type contextKey string

const ClientIDContextKey = contextKey("clientID")

// AuthMiddleware validates the JWT from the Authorization header.
func AuthMiddleware(app *types.App) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				errors.WriteJSONError(w, http.StatusUnauthorized, "Authorization header is required")
				return
			}

			parts := strings.Split(authHeader, " ")
			if len(parts) != 2 || parts[0] != "Bearer" {
				errors.WriteJSONError(w, http.StatusUnauthorized, "Authorization header format must be Bearer {token}")
				return
			}

			tokenString := parts[1]
			token, err := auth.ValidateJWT(tokenString, &app.PrivateKey.PublicKey)
			if err != nil {
				errors.WriteJSONError(w, http.StatusUnauthorized, "Invalid token")
				return
			}

			if !token.Valid {
				errors.WriteJSONError(w, http.StatusUnauthorized, "Invalid token")
				return
			}

			// Add clientID to the context for use in downstream handlers
			claims, ok := token.Claims.(*auth.JWTClaims)
			if !ok {
				errors.WriteJSONError(w, http.StatusUnauthorized, "Invalid token claims")
				return
			}

			ctx := context.WithValue(r.Context(), ClientIDContextKey, claims.ClientID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AuthorizeMiddleware checks if the requested project is authorized for the client.
func AuthorizeMiddleware(app *types.App) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			app.Log.Info("AuthorizeMiddleware called")
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				var maxBytesErr *http.MaxBytesError
				if stderrors.As(err, &maxBytesErr) {
					errors.WriteJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
					return
				}
				errors.WriteJSONError(w, http.StatusInternalServerError, "Failed to read request body")
				return
			}
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

			var req struct {
				Project      string `json:"project"`
				ClientSecret string `json:"clientsecret"`
			}

			if err := json.Unmarshal(bodyBytes, &req); err != nil {
				errors.WriteJSONError(w, http.StatusBadRequest, "Invalid request body")
				return
			}

			if !auth.ValidateClientRequest(r.Context(), app, req.Project, req.ClientSecret) {
				errors.WriteJSONError(w, http.StatusForbidden, "Client not authorized")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// BodyLimitMiddleware caps the size of a request body so that a chunked or
// lying client cannot stream unbounded data into memory (json.Decode and
// io.ReadAll in the handlers read through this wrapper).
func BodyLimitMiddleware(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > maxBytes {
				errors.WriteJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimitKeyFunc derives the rate-limit bucket for a request. Requests
// mapping to the same key share one token bucket, so one key's request volume
// never consumes another key's budget.
type RateLimitKeyFunc func(*http.Request) string

// ClientIDRateLimitKey buckets by the authenticated client ID, isolating
// tenants from each other. It must be installed after AuthMiddleware; a
// request without a client ID in its context falls back to its peer address.
func ClientIDRateLimitKey(r *http.Request) string {
	if clientID, ok := r.Context().Value(ClientIDContextKey).(string); ok && clientID != "" {
		return "client:" + clientID
	}
	return RemoteIPRateLimitKey(r)
}

// RemoteIPRateLimitKey buckets by the TCP peer address, for unauthenticated
// routes where no client identity exists. X-Forwarded-For is deliberately
// ignored: the API server is reached directly, and trusting a caller-supplied
// header would let anyone mint unlimited fresh buckets.
func RemoteIPRateLimitKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "ip:" + r.RemoteAddr
	}
	return "ip:" + host
}

// GlobalRateLimitKey puts every request in one bucket, i.e. an aggregate
// ceiling across all keys. It is a backstop, not the fairness mechanism: the
// client ID is chosen by the caller at token issuance (TokenHandler accepts
// any clientID without authentication, and authorization binds the project
// secret, not the client ID), so a caller can mint fresh client IDs and with
// them fresh per-client buckets. The ceiling bounds that churn; it must sit
// well above normal aggregate traffic so tenants do not contend on it.
func GlobalRateLimitKey(*http.Request) string { return "global" }

const (
	// limiterIdleTTL is how long an unused bucket is kept. Long enough that a
	// client polling every few minutes keeps its budget between requests.
	limiterIdleTTL = 10 * time.Minute
	// limiterSweepInterval bounds how often the whole bucket map is scanned.
	limiterSweepInterval = time.Minute
	// limiterMaxEntries caps memory when the key space churns (many distinct
	// source addresses); at the cap the least recently seen buckets are
	// dropped, which only costs those keys a fresh burst.
	limiterMaxEntries = 10000
)

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// limiterStore hands out one token bucket per key and forgets buckets idle for
// limiterIdleTTL, so per-client limiting does not leak memory as clients come
// and go. rate.Limiter is safe for concurrent use, so Allow runs outside the
// lock.
type limiterStore struct {
	mu        sync.Mutex
	entries   map[string]*limiterEntry
	limit     rate.Limit
	burst     int
	now       func() time.Time
	lastSweep time.Time
}

func newLimiterStore(requestsPerSecond float64, burst int) *limiterStore {
	return &limiterStore{
		entries: make(map[string]*limiterEntry),
		limit:   rate.Limit(requestsPerSecond),
		burst:   burst,
		now:     time.Now,
	}
}

// allow consumes one token from key's bucket and reports whether the request
// may proceed.
func (s *limiterStore) allow(key string) bool {
	s.mu.Lock()
	now := s.now()
	if now.Sub(s.lastSweep) >= limiterSweepInterval {
		s.sweepLocked(now)
	}
	entry, ok := s.entries[key]
	if !ok {
		if len(s.entries) >= limiterMaxEntries {
			s.evictOldestLocked()
		}
		entry = &limiterEntry{limiter: rate.NewLimiter(s.limit, s.burst)}
		s.entries[key] = entry
	}
	entry.lastSeen = now
	s.mu.Unlock()

	return entry.limiter.Allow()
}

func (s *limiterStore) sweepLocked(now time.Time) {
	s.lastSweep = now
	for key, entry := range s.entries {
		if now.Sub(entry.lastSeen) > limiterIdleTTL {
			delete(s.entries, key)
		}
	}
}

// evictOldestLocked drops the least recently seen eighth of the buckets in one
// pass, so a churning key space pays the O(n log n) sort once per batch
// instead of once per new key.
func (s *limiterStore) evictOldestLocked() {
	target := len(s.entries) / 8
	if target < 1 {
		target = 1
	}
	type entryAge struct {
		key      string
		lastSeen time.Time
	}
	ages := make([]entryAge, 0, len(s.entries))
	for key, entry := range s.entries {
		ages = append(ages, entryAge{key: key, lastSeen: entry.lastSeen})
	}
	sort.Slice(ages, func(i, j int) bool { return ages[i].lastSeen.Before(ages[j].lastSeen) })
	for _, age := range ages[:target] {
		delete(s.entries, age.key)
	}
}

// RateLimitMiddleware applies per-key rate limiting to incoming requests: each
// key gets its own requestsPerSecond/burst bucket, so a noisy client cannot
// exhaust every other client's budget.
func RateLimitMiddleware(requestsPerSecond float64, burst int, keyFn RateLimitKeyFunc) func(http.Handler) http.Handler {
	return rateLimitHandler(newLimiterStore(requestsPerSecond, burst), keyFn)
}

func rateLimitHandler(store *limiterStore, keyFn RateLimitKeyFunc) func(http.Handler) http.Handler {
	if keyFn == nil {
		keyFn = RemoteIPRateLimitKey
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !store.allow(keyFn(r)) {
				errors.WriteJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
