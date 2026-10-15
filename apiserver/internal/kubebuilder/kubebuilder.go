// Package kubebuilder provides utilities for interacting with Kubernetes custom resources.
package kubebuilder

import (
	"context"
	"time"

	"github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
)

const (
	// floatingIPGroupLabel is the label key used to identify floating IP group requests.
	floatingIPGroupLabel = "rancher.k8s.binbash.org/floatingip-group"
	// floatingIPResource is the plural resource name of FloatingIP.
	floatingIPResource = "floatingips"
	// watchReconnectInitialDelay and watchReconnectMaxDelay bound the backoff
	// applied when a Get or Watch attempt fails transiently, so a hopeless
	// request cannot hammer the API server.
	watchReconnectInitialDelay = 200 * time.Millisecond
	watchReconnectMaxDelay     = 2 * time.Second
)

// NewFipRestClient creates a new REST client for FloatingIP custom resources.
func NewFipRestClient(config *rest.Config, scheme *runtime.Scheme) (*rest.RESTClient, error) {
	crdConfig := *config
	crdConfig.ContentConfig.GroupVersion = &v1beta2.SchemeGroupVersion
	crdConfig.APIPath = "/apis"
	crdConfig.NegotiatedSerializer = serializer.NewCodecFactory(scheme)
	crdConfig.UserAgent = rest.DefaultKubernetesUserAgent()

	restClient, err := rest.UnversionedRESTClientFor(&crdConfig)
	if err != nil {
		return nil, err
	}

	return restClient, nil
}

// WatchFloatingIP watches a FloatingIP resource until its status contains an IP address.
// It uses a server-side watch (with reconnect and a fresh Get after every
// reconnect to close watch gaps) instead of high-frequency polling.
// If the floatingIPGroupLabel is set on the FloatingIP, it waits until
// Status.Assigned.FloatingIPGroup is also populated before returning.
//
// Definitive errors (NotFound, Forbidden) are returned immediately; transient
// errors are retried with backoff until the caller's context expires, in which
// case ctx.Err() is returned.
func WatchFloatingIP(ctx context.Context, cl *rest.RESTClient, currentFip *v1beta2.FloatingIP) (*v1beta2.FloatingIP, error) {
	waitForFloatingIPGroup := currentFip.Labels != nil && currentFip.Labels[floatingIPGroupLabel] != ""

	allocated := func(fip *v1beta2.FloatingIP) bool {
		if fip.Status.IPAddr == "" {
			return false
		}
		if waitForFloatingIPGroup {
			return fip.Status.Assigned != nil && fip.Status.Assigned.FloatingIPGroup != ""
		}
		return true
	}

	delay := watchReconnectInitialDelay
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Fresh Get each iteration: serves as the initial read and closes any
		// gap created by a dropped/re-established watch.
		fip, err := getFloatingIP(ctx, cl, currentFip.Namespace, currentFip.Name)
		if err != nil {
			if isTerminalWatchError(err) {
				return nil, err
			}
			if !sleepWithContext(ctx, delay) {
				return nil, ctx.Err()
			}
			delay = nextReconnectDelay(delay)
			continue
		}
		if allocated(fip) {
			return fip, nil
		}

		w, err := openFloatingIPWatch(ctx, cl, currentFip.Namespace, currentFip.Name, fip.ResourceVersion)
		if err != nil {
			if isTerminalWatchError(err) {
				return nil, err
			}
			if !sleepWithContext(ctx, delay) {
				return nil, ctx.Err()
			}
			delay = nextReconnectDelay(delay)
			continue
		}

		result, watchErr := consumeWatch(ctx, w, allocated)
		if watchErr != nil {
			if isTerminalWatchError(watchErr) {
				return nil, watchErr
			}
			// Stale resource version (410) or undecodable event: fall through
			// to the reconnect path; the next Get refreshes state and version.
		}
		if result != nil {
			return result, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		delay = nextReconnectDelay(delay)
	}
}

// getFloatingIP performs a single Get of a FloatingIP via the REST client.
func getFloatingIP(ctx context.Context, cl *rest.RESTClient, namespace, name string) (*v1beta2.FloatingIP, error) {
	fip := &v1beta2.FloatingIP{}
	if err := cl.Get().Namespace(namespace).Resource(floatingIPResource).Name(name).Do(ctx).Into(fip); err != nil {
		return nil, err
	}
	return fip, nil
}

// openFloatingIPWatch opens a server-side watch on a single FloatingIP,
// continuing from resourceVersion.
func openFloatingIPWatch(ctx context.Context, cl *rest.RESTClient, namespace, name, resourceVersion string) (watch.Interface, error) {
	return cl.Get().
		Namespace(namespace).
		Resource(floatingIPResource).
		Name(name).
		VersionedParams(&metav1.ListOptions{ResourceVersion: resourceVersion}, metav1.ParameterCodec).
		Watch(ctx)
}

// consumeWatch reads watch events until the allocation condition is satisfied,
// the watch terminates (closed or error event), or ctx is done.
func consumeWatch(ctx context.Context, w watch.Interface, allocated func(*v1beta2.FloatingIP) bool) (*v1beta2.FloatingIP, error) {
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		case event, ok := <-w.ResultChan():
			if !ok {
				// Watch closed by the server; caller reconnects.
				return nil, nil
			}
			if event.Type == watch.Error {
				if event.Object != nil {
					return nil, apierrors.FromObject(event.Object)
				}
				return nil, nil
			}
			fip, ok := event.Object.(*v1beta2.FloatingIP)
			if !ok || fip == nil {
				continue
			}
			if allocated(fip) {
				return fip, nil
			}
		}
	}
}

// isTerminalWatchError reports whether err is a definitive rejection that
// cannot be resolved by waiting: retrying it burns API-server capacity for a
// request that can never succeed.
func isTerminalWatchError(err error) bool {
	return apierrors.IsNotFound(err) || apierrors.IsForbidden(err)
}

// sleepWithContext waits for delay, returning false if ctx is done first.
func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// nextReconnectDelay doubles delay, capped at watchReconnectMaxDelay.
func nextReconnectDelay(delay time.Duration) time.Duration {
	delay *= 2
	if delay > watchReconnectMaxDelay {
		return watchReconnectMaxDelay
	}
	return delay
}
