package util

import (
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
)

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// GenerateRandomString generates a random string of the specified length
// using characters from [a-z][A-Z][0-9].
//
// A crypto/rand failure is returned as an error instead of being swallowed:
// the previous fallback indexed charset out of range (guaranteed panic), and
// any predictable fallback would silently produce collidable shared keys.
func GenerateRandomString(length int) (string, error) {
	return generateRandomStringFrom(rand.Reader, length)
}

func generateRandomStringFrom(r io.Reader, length int) (string, error) {
	max := big.NewInt(int64(len(charset)))
	result := make([]byte, length)
	for i := range result {
		// Generate a random index into the charset; rand.Int returns [0, max).
		n, err := rand.Int(r, max)
		if err != nil {
			return "", fmt.Errorf("failed to read random entropy for string generation: %w", err)
		}
		result[i] = charset[n.Int64()]
	}
	return string(result), nil
}
