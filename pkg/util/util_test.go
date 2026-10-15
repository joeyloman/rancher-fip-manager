package util

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateRandomString(t *testing.T) {
	s, err := GenerateRandomString(12)
	require.NoError(t, err)
	require.Len(t, s, 12)
	for _, c := range s {
		assert.Contains(t, charset, string(c), "generated char must come from the charset")
	}
}

// failingReader simulates a crypto/rand entropy source that errors.
type failingReader struct{}

func (failingReader) Read(p []byte) (int, error) {
	return 0, errors.New("entropy source failed")
}

// Regression for report 1.4: a crypto/rand failure used to fall back to
// charset[62] — an out-of-range index, i.e. a guaranteed panic.
func TestGenerateRandomStringFromFailingReader(t *testing.T) {
	s, err := generateRandomStringFrom(failingReader{}, 8)
	require.Error(t, err)
	assert.Empty(t, s)
	assert.Contains(t, err.Error(), "entropy source failed")
}
