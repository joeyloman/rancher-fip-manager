package controllers

import (
	"context"
	"fmt"
	"testing"

	"github.com/joeyloman/rancher-fip-cluster-manager/internal/helm"
	"github.com/joeyloman/rancher-fip-cluster-manager/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"helm.sh/helm/v3/pkg/repo"
)

// stubHelmClient records the charts the provisioning path asked Helm to
// install; installErr lets a test stop the reconcile right after that call.
type stubHelmClient struct {
	installed  []string
	installErr error
}

func (s *stubHelmClient) InstallOrUpgrade(ctx context.Context, config *rest.Config, spec helm.ChartSpec) error {
	s.installed = append(s.installed, spec.ReleaseName)
	return s.installErr
}

func (s *stubHelmClient) AddOrUpdateChartRepo(entry repo.Entry) error { return nil }

func (s *stubHelmClient) CheckIfChartIsInstalled(ctx context.Context, config *rest.Config, releaseName, namespace string) (bool, error) {
	return false, nil
}

const testDownstreamKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: downstream
contexts:
- context:
    cluster: downstream
    user: downstream
  name: downstream
current-context: downstream
users:
- name: downstream
  user: {}
`

// TestReconcileRetriesAfterBareError is the regression for the gate that used
// to remember the rancher-fip label value per cluster in process memory. A
// reconcile that failed with a plain error left that memory armed, so the
// backoff retry classified the cluster as an unchanged UPDATE event and
// returned nil without installing anything: provisioning was silently
// abandoned until some unrelated event touched the Cluster object.
func TestReconcileRetriesAfterBareError(t *testing.T) {
	scheme := newTestScheme(t)

	cluster := fipCluster(map[string]string{fipEnabledLabel: "enabled"})
	kubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name + "-kubeconfig", Namespace: "fleet-default"},
		Data:       map[string][]byte{"value": []byte(testDownstreamKubeconfig)},
	}

	var failSecretList bool
	local := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster, kubeconfigSecret).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if failSecretList {
					if _, isSecretList := list.(*corev1.SecretList); isSecretList {
						return fmt.Errorf("api server unavailable")
					}
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	downstream := fake.NewClientBuilder().WithScheme(scheme).Build()
	helmStub := &stubHelmClient{installErr: fmt.Errorf("stop after the first chart")}

	r := &ClusterProvisioningReconciler{
		Client:       local,
		Scheme:       scheme,
		HelmClient:   helmStub,
		Config:       &config.Config{RancherFipLBControllerChartName: "rancher-fip-lb-controller", RancherFipLBControllerNamespace: "rancher-fip-lb-controller"},
		AppNamespace: "rancher-fip-manager",
		NewDownstreamClientFunc: func(*rest.Config) (client.Client, error) {
			return downstream, nil
		},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}
	ctx := context.Background()

	// First attempt: a transient API failure must surface as an error so
	// controller-runtime requeues with backoff.
	failSecretList = true
	result, err := r.Reconcile(ctx, req)
	require.Error(t, err, "a failing API call must be returned, not swallowed")
	assert.Zero(t, result.RequeueAfter)
	assert.Empty(t, helmStub.installed, "the failing attempt must not have installed anything")

	// The retry must do the work instead of being filtered against
	// remembered state.
	failSecretList = false
	_, err = r.Reconcile(ctx, req)
	require.Error(t, err, "the stub stops the path on purpose")
	assert.Equal(t, []string{"rancher-fip-lb-controller"}, helmStub.installed,
		"the retry after a bare error must re-run the Helm phase")
}
