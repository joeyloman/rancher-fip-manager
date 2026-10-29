package purelb

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Client is a client for the PureLB API.
type Client struct {
	client client.Client
}

// NewClient creates a new PureLB client.
func NewClient(config *rest.Config) (*Client, error) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		return nil, err
	}

	cl, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}

	return &Client{client: cl}, nil
}

// GetServiceGroups returns a list with all ServiceGroups.
func (c *Client) GetServiceGroups(ctx context.Context, namespace string) ([]ServiceGroup, error) {
	groups := &ServiceGroupList{}
	err := c.client.List(ctx, groups, client.InNamespace(namespace))
	if err != nil {
		return nil, err
	}
	return groups.Items, nil
}

// CreateServiceGroup creates a ServiceGroup.
func (c *Client) CreateServiceGroup(ctx context.Context, group *ServiceGroup) error {
	return c.client.Create(ctx, group)
}

// CreateLBNodeAgent creates an LBNodeAgent.
func (c *Client) CreateLBNodeAgent(ctx context.Context, agent *LBNodeAgent) error {
	return c.client.Create(ctx, agent)
}

// DeleteServiceGroup deletes a ServiceGroup.
func (c *Client) DeleteServiceGroup(ctx context.Context, name, namespace string) error {
	group := &ServiceGroup{}
	group.Name = name
	group.Namespace = namespace
	return c.client.Delete(ctx, group)
}

// DeleteLBNodeAgent deletes an LBNodeAgent.
func (c *Client) DeleteLBNodeAgent(ctx context.Context, name, namespace string) error {
	agent := &LBNodeAgent{}
	agent.Name = name
	agent.Namespace = namespace
	return c.client.Delete(ctx, agent)
}
