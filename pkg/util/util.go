package util

import (
	"crypto/rand"
	"math/big"
)

// generateRandomString generates a random string of the specified length
// using characters from [a-z][A-Z][0-9].
func GenerateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		// Generate a random index into the charset
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			// Fallback to simple random if crypto/rand fails
			n = big.NewInt(int64(len(charset)))
		}
		result[i] = charset[n.Int64()]
	}
	return string(result)
}
