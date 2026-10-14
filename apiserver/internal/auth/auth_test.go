package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestJWTAuthentication(t *testing.T) {
	// Generate a new RSA private key for testing.
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "failed to generate RSA private key")

	publicKey := &privateKey.PublicKey

	const clientID = "test-client"
	const expiration = time.Minute * 15

	// Test case 1: Successful JWT generation and validation.
	t.Run("Successful JWT Generation and Validation", func(t *testing.T) {
		signedToken, _, err := GenerateJWT(privateKey, clientID, expiration)
		require.NoError(t, err, "failed to generate JWT")

		token, err := ValidateJWT(signedToken, publicKey)
		require.NoError(t, err, "failed to validate JWT")

		assert.True(t, token.Valid, "token should be valid")

		claims, ok := token.Claims.(*JWTClaims)
		require.True(t, ok, "failed to parse claims")

		assert.Equal(t, clientID, claims.ClientID, "ClientID should match")
	})

	// Test case 2: Validation of an invalid token.
	t.Run("Validation of an Invalid Token", func(t *testing.T) {
		invalidTokenString := "this.is.an.invalid.token"
		_, err := ValidateJWT(invalidTokenString, publicKey)
		assert.Error(t, err, "validation should fail for an invalid token")
	})

	// Test case 3: Token signed with a different key.
	t.Run("Token Signed with Different Key", func(t *testing.T) {
		// Generate another private key.
		anotherPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)

		signedToken, _, err := GenerateJWT(privateKey, clientID, expiration)
		require.NoError(t, err)

		// Try to validate with the wrong public key.
		_, err = ValidateJWT(signedToken, &anotherPrivateKey.PublicKey)
		assert.Error(t, err, "validation should fail for token signed with a different key")
	})

	// Test case 4: Expired token validation.
	t.Run("Expired Token Validation", func(t *testing.T) {
		// Create an already expired token.
		claims := JWTClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
			},
			ClientID: clientID,
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		signedToken, err := token.SignedString(privateKey)
		require.NoError(t, err)

		_, err = ValidateJWT(signedToken, publicKey)
		assert.Error(t, err, "validation should fail for an expired token")
	})
}

func TestEnsurePrivateKey(t *testing.T) {
	const ns, name = "rancher-fip-manager", "jwt-private-key"

	winnerKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	winnerSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string][]byte{
			"key": pem.EncodeToMemory(&pem.Block{
				Type:  "RSA PRIVATE KEY",
				Bytes: x509.MarshalPKCS1PrivateKey(winnerKey),
			}),
		},
	}

	t.Run("Adopts Winner Key on Create Race", func(t *testing.T) {
		// Replicates the first-boot race: this replica's first Get sees
		// NotFound, another replica creates the Secret before our Create
		// lands, so Create fails AlreadyExists and we must re-fetch and
		// adopt the winner's key (pre-fix: log.Fatalf -> crashloop).
		client := fake.NewSimpleClientset(winnerSecret)
		var getCalls, createCalls int
		client.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
			getCalls++
			if getCalls == 1 {
				return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), name)
			}
			return false, nil, nil
		})
		client.PrependReactor("create", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
			createCalls++
			return true, nil, apierrors.NewAlreadyExists(corev1.Resource("secrets"), name)
		})

		got, err := EnsurePrivateKey(context.Background(), client, ns, name, true)
		require.NoError(t, err, "create-race loser must not error")
		assert.Equal(t, 1, createCalls, "must have attempted to create the secret")
		assert.Equal(t, 2, getCalls, "must have re-fetched after AlreadyExists")
		assert.Equal(t, winnerKey, got, "must adopt the winner's key, not its own")
	})

	t.Run("Returns Existing Key Without Create", func(t *testing.T) {
		client := fake.NewSimpleClientset(winnerSecret)
		got, err := EnsurePrivateKey(context.Background(), client, ns, name, true)
		require.NoError(t, err)
		assert.Equal(t, winnerKey, got)
	})

	t.Run("Missing Key With Generation Disabled Errors", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		_, err := EnsurePrivateKey(context.Background(), client, ns, name, false)
		require.Error(t, err)
		assert.ErrorContains(t, err, "generation is disabled")
	})
}
