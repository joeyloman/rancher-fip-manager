package floatingip

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression for report 1.4: a panicking syncHandler must be recovered inside
// the worker and converted into a rate-limited requeue. Before the fix the
// panic propagated out of the worker goroutine through wait.Until and killed
// the whole manager process — while it held the leader lease.
func TestProcessNextWorkItemRecoversSyncHandlerPanic(t *testing.T) {
	f := newFixture(t, nil, nil)
	f.controller.syncHandler = func(ctx context.Context, key string) error {
		panic("boom")
	}

	f.controller.workqueue.Add("test-ns/test-fip")
	require.True(t, f.controller.processNextWorkItem(context.Background()),
		"worker must keep running after a panicking reconcile")
	assert.Equal(t, 1, f.controller.workqueue.NumRequeues("test-ns/test-fip"),
		"panicked key must be requeued with rate limiting")
}
