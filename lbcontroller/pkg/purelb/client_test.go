package purelb

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestClient(initObjs ...client.Object) (*Client, error) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		return nil, err
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()
	return &Client{client: cl}, nil
}

func TestCreateServiceGroup(t *testing.T) {
	group := &ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-group",
			Namespace: "default",
		},
		Spec: ServiceGroupSpec{
			Local: &ServiceGroupLocal{
				V4Pools: []AddressPool{
					{
						Subnet: "192.168.1.0/24",
						Pool:   "192.168.1.1-192.168.1.10",
					},
				},
			},
		},
	}

	c, err := newTestClient()
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.CreateServiceGroup(context.Background(), group); err != nil {
		t.Fatalf("CreateServiceGroup() error = %v, wantErr nil", err)
	}

	// Verify the group was created
	createdGroup := &ServiceGroup{}
	err = c.client.Get(context.Background(), client.ObjectKey{
		Name:      "test-group",
		Namespace: "default",
	}, createdGroup)
	if err != nil {
		t.Fatalf("failed to get created group: %v", err)
	}
	if createdGroup.Spec.Local == nil || len(createdGroup.Spec.Local.V4Pools) == 0 {
		t.Errorf("unexpected group spec: got %v", createdGroup.Spec)
	}
	if createdGroup.Spec.Local.V4Pools[0].Pool != "192.168.1.1-192.168.1.10" {
		t.Errorf("unexpected pool: got %v, want %v", createdGroup.Spec.Local.V4Pools[0].Pool, "192.168.1.1-192.168.1.10")
	}
}

func TestCreateLBNodeAgent(t *testing.T) {
	sendGARP := true
	agent := &LBNodeAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: LBNodeAgentSpec{
			Local: &LBNodeAgentLocal{
				ExtLBInt: "kube-lb0",
				LocalInt: "eth0",
				SendGARP: &sendGARP,
			},
		},
	}

	c, err := newTestClient()
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.CreateLBNodeAgent(context.Background(), agent); err != nil {
		t.Fatalf("CreateLBNodeAgent() error = %v, wantErr nil", err)
	}

	// Verify the agent was created
	createdAgent := &LBNodeAgent{}
	err = c.client.Get(context.Background(), client.ObjectKey{
		Name:      "test-agent",
		Namespace: "default",
	}, createdAgent)
	if err != nil {
		t.Fatalf("failed to get created agent: %v", err)
	}
	if createdAgent.Spec.Local == nil {
		t.Errorf("unexpected agent spec: got %v", createdAgent.Spec)
	}
	if createdAgent.Spec.Local.ExtLBInt != "kube-lb0" {
		t.Errorf("unexpected extlbint: got %v, want %v", createdAgent.Spec.Local.ExtLBInt, "kube-lb0")
	}
}

func TestDeleteServiceGroup(t *testing.T) {
	group := &ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-group",
			Namespace: "default",
		},
	}

	c, err := newTestClient(group)
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.DeleteServiceGroup(context.Background(), "test-group", "default"); err != nil {
		t.Fatalf("DeleteServiceGroup() error = %v, wantErr nil", err)
	}

	// Verify the group was deleted
	deletedGroup := &ServiceGroup{}
	err = c.client.Get(context.Background(), client.ObjectKey{
		Name:      "test-group",
		Namespace: "default",
	}, deletedGroup)
	if err == nil {
		t.Fatal("expected group to be deleted, but it still exists")
	}
}

func TestDeleteLBNodeAgent(t *testing.T) {
	agent := &LBNodeAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
	}

	c, err := newTestClient(agent)
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.DeleteLBNodeAgent(context.Background(), "test-agent", "default"); err != nil {
		t.Fatalf("DeleteLBNodeAgent() error = %v, wantErr nil", err)
	}

	// Verify the agent was deleted
	deletedAgent := &LBNodeAgent{}
	err = c.client.Get(context.Background(), client.ObjectKey{
		Name:      "test-agent",
		Namespace: "default",
	}, deletedAgent)
	if err == nil {
		t.Fatal("expected agent to be deleted, but it still exists")
	}
}

func TestGetServiceGroups(t *testing.T) {
	group1 := &ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-group-1",
			Namespace: "default",
		},
	}
	group2 := &ServiceGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-group-2",
			Namespace: "default",
		},
	}

	c, err := newTestClient(group1, group2)
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	groups, err := c.GetServiceGroups(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetServiceGroups() error = %v, wantErr nil", err)
	}

	if len(groups) != 2 {
		t.Errorf("GetServiceGroups() returned %d groups, want 2", len(groups))
	}
}

func TestDeleteNonExistentServiceGroup(t *testing.T) {
	c, err := newTestClient()
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.DeleteServiceGroup(context.Background(), "test-group", "default"); err == nil {
		t.Fatal("DeleteServiceGroup() error = nil, wantErr not nil")
	}
}

func TestDeleteNonExistentLBNodeAgent(t *testing.T) {
	c, err := newTestClient()
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	if err := c.DeleteLBNodeAgent(context.Background(), "test-agent", "default"); err == nil {
		t.Fatal("DeleteLBNodeAgent() error = nil, wantErr not nil")
	}
}
