// Package controllerutil holds helpers shared by the hand-rolled controllers
// under pkg/controller.
package controllerutil

import (
	"fmt"
	"runtime"
)

// maxStackBytes bounds the stack trace captured for a recovered reconcile
// panic. Deeper frames than this are truncated; the top frames are the ones
// that identify the panicking syncHandler.
const maxStackBytes = 8 << 10

// HandleSyncPanic converts a recovered panic value from a reconcile attempt
// into an error, including the stack trace of the panicking goroutine. The
// controllers call it from a deferred recover inside processNextWorkItem so a
// panicking syncHandler becomes a rate-limited retry instead of a
// process-fatal crash (which would also drop the leader lease holder).
//
// It returns nil when r is nil, i.e. when no panic occurred.
func HandleSyncPanic(key string, r interface{}) error {
	if r == nil {
		return nil
	}
	stack := make([]byte, maxStackBytes)
	n := runtime.Stack(stack, false)
	return fmt.Errorf("panic while syncing %q: %v\n%s", key, r, stack[:n])
}
