package secret

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateClientSecret(t *testing.T) {
	token, err := GenerateClientSecret()
	assert.NoError(t, err)
	assert.Len(t, token, 32)
}

func TestNewSecret(t *testing.T) {
	name := "test-secret"
	namespace := "default"
	apiUrl := "https://rancher-api.localdomain"
	clientSecret := "test-clientSecret"
	cluster := "test-cluster"
	project := "test-project"
	floatingIPPool := "test-floatingIPPool"
	loadBalancerType := "purelb"

	secret := NewDownstreamSecret(name, namespace, apiUrl, clientSecret, cluster, project, floatingIPPool, loadBalancerType)

	assert.Equal(t, name, secret.Name)
	assert.Equal(t, namespace, secret.Namespace)
	assert.Equal(t, []byte(apiUrl), secret.Data["apiUrl"])
	assert.Equal(t, []byte(clientSecret), secret.Data["clientSecret"])
	assert.Equal(t, []byte(cluster), secret.Data["cluster"])
	assert.Equal(t, []byte(project), secret.Data["project"])
	assert.Equal(t, []byte(floatingIPPool), secret.Data["floatingIPPool"])
	assert.Equal(t, []byte(loadBalancerType), secret.Data["loadBalancerType"])
}

func TestNewLocalSecret(t *testing.T) {
	name := "test-secret"
	namespace := "default"
	clientSecret := "test-clientSecret"
	project := "test-project"

	secret := NewLocalSecret(name, namespace, clientSecret, project)

	assert.Equal(t, name, secret.Name)
	assert.Equal(t, namespace, secret.Namespace)
	assert.Equal(t, []byte(clientSecret), secret.Data["clientSecret"])
	assert.Equal(t, []byte(project), secret.Data["project"])
}
