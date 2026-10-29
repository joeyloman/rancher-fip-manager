package metallb

import (
	"context"
	"testing"

	metallbv1beta1 "go.universe.tf/metallb/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestClient(initObjs ...client.Object) (*Client, error) {
	scheme := runtime.NewScheme()
	if err := metallbv1beta1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()
	return &Client{client: cl}, nil
}

func TestCreateIPAddressPool(t *testing.T) {
	pool := &metallbv1beta1.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "default",
		},
		Spec: metallbv1beta1.IPAddressPoolSpec{
			Addresses: []string{"192.168.1.1-192.168.1.10"},
		},
	}

	c, err := newTestClient()
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.CreateIPAddressPool(context.Background(), pool); err != nil {
		t.Fatalf("CreateIPAddressPool() error = %v, wantErr nil", err)
	}

	// Verify the pool was created
	createdPool := &metallbv1beta1.IPAddressPool{}
	err = c.client.Get(context.Background(), client.ObjectKey{
		Name:      "test-pool",
		Namespace: "default",
	}, createdPool)
	if err != nil {
		t.Fatalf("failed to get created pool: %v", err)
	}
	if createdPool.Spec.Addresses[0] != "192.168.1.1-192.168.1.10" {
		t.Errorf("unexpected pool addresses: got %v, want %v", createdPool.Spec.Addresses[0], "192.168.1.1-192.168.1.10")
	}
}

func TestCreateL2Advertisement(t *testing.T) {
	ad := &metallbv1beta1.L2Advertisement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ad",
			Namespace: "default",
		},
	}

	c, err := newTestClient()
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.CreateL2Advertisement(context.Background(), ad); err != nil {
		t.Fatalf("CreateL2Advertisement() error = %v, wantErr nil", err)
	}

	// Verify the advertisement was created
	createdAd := &metallbv1beta1.L2Advertisement{}
	err = c.client.Get(context.Background(), client.ObjectKey{
		Name:      "test-ad",
		Namespace: "default",
	}, createdAd)
	if err != nil {
		t.Fatalf("failed to get created advertisement: %v", err)
	}
}

func TestDeleteIPAddressPool(t *testing.T) {
	pool := &metallbv1beta1.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "default",
		},
	}

	c, err := newTestClient(pool)
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.DeleteIPAddressPool(context.Background(), "test-pool", "default"); err != nil {
		t.Fatalf("DeleteIPAddressPool() error = %v, wantErr nil", err)
	}

	// Verify the pool was deleted
	deletedPool := &metallbv1beta1.IPAddressPool{}
	err = c.client.Get(context.Background(), client.ObjectKey{
		Name:      "test-pool",
		Namespace: "default",
	}, deletedPool)
	if err == nil {
		t.Fatal("expected pool to be deleted, but it still exists")
	}
}

func TestDeleteL2Advertisement(t *testing.T) {
	ad := &metallbv1beta1.L2Advertisement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ad",
			Namespace: "default",
		},
	}

	c, err := newTestClient(ad)
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.DeleteL2Advertisement(context.Background(), "test-ad", "default"); err != nil {
		t.Fatalf("DeleteL2Advertisement() error = %v, wantErr nil", err)
	}

	// Verify the advertisement was deleted
	deletedAd := &metallbv1beta1.L2Advertisement{}
	err = c.client.Get(context.Background(), client.ObjectKey{
		Name:      "test-ad",
		Namespace: "default",
	}, deletedAd)
	if err == nil {
		t.Fatal("expected advertisement to be deleted, but it still exists")
	}
}

func TestDeleteNonExistentIPAddressPool(t *testing.T) {
	c, err := newTestClient()
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.DeleteIPAddressPool(context.Background(), "test-pool", "default"); err == nil {
		t.Fatal("DeleteIPAddressPool() error = nil, wantErr not nil")
	}
}

func TestDeleteNonExistentL2Advertisement(t *testing.T) {
	c, err := newTestClient()
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.DeleteL2Advertisement(context.Background(), "test-ad", "default"); err == nil {
		t.Fatal("DeleteL2Advertisement() error = nil, wantErr not nil")
	}
}
