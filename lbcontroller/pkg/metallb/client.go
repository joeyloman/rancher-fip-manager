package metallb

import (
	"context"

	metallbv1beta1 "go.universe.tf/metallb/api/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Client is a client for the MetalLB API.
type Client struct {
	client client.Client
}

// NewClient creates a new MetalLB client.
func NewClient(config *rest.Config) (*Client, error) {
	scheme := runtime.NewScheme()
	if err := metallbv1beta1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	cl, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}

	return &Client{client: cl}, nil
}

// GetIPAddressPools gets all IPAddressPools.
func (c *Client) GetIPAddressPools(ctx context.Context, namespace string) ([]metallbv1beta1.IPAddressPool, error) {
	pools := &metallbv1beta1.IPAddressPoolList{}
	err := c.client.List(ctx, pools, client.InNamespace(namespace))
	if err != nil {
		return nil, err
	}
	return pools.Items, nil
}

// CreateIPAddressPool creates an IPAddressPool.
func (c *Client) CreateIPAddressPool(ctx context.Context, pool *metallbv1beta1.IPAddressPool) error {
	return c.client.Create(ctx, pool)
}

// CreateL2Advertisement creates an L2Advertisement.
func (c *Client) CreateL2Advertisement(ctx context.Context, ad *metallbv1beta1.L2Advertisement) error {
	return c.client.Create(ctx, ad)
}

// GetIPAddressPool gets an IPAddressPool by name.
func (c *Client) GetIPAddressPool(ctx context.Context, name, namespace string) (*metallbv1beta1.IPAddressPool, error) {
	pool := &metallbv1beta1.IPAddressPool{}
	pool.Name = name
	pool.Namespace = namespace
	err := c.client.Get(ctx, client.ObjectKeyFromObject(pool), pool)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// DeleteIPAddressPool deletes an IPAddressPool.
func (c *Client) DeleteIPAddressPool(ctx context.Context, name, namespace string) error {
	pool := &metallbv1beta1.IPAddressPool{}
	pool.Name = name
	pool.Namespace = namespace
	return c.client.Delete(ctx, pool)
}

// UpdateIPAddressPool updates an IPAddressPool.
func (c *Client) UpdateIPAddressPool(ctx context.Context, pool *metallbv1beta1.IPAddressPool) error {
	return c.client.Update(ctx, pool)
}

// DeleteL2Advertisement deletes an L2Advertisement.
func (c *Client) DeleteL2Advertisement(ctx context.Context, name, namespace string) error {
	ad := &metallbv1beta1.L2Advertisement{}
	ad.Name = name
	ad.Namespace = namespace
	return c.client.Delete(ctx, ad)
}
