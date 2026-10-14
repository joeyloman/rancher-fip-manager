package middleware

import (
	"bytes"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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
