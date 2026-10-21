// Package handlers implements the HTTP handlers for the API endpoints.
package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joeyloman/rancher-fip-api-server/internal/auth"
	"github.com/joeyloman/rancher-fip-api-server/internal/errors"
	"github.com/joeyloman/rancher-fip-api-server/internal/kubebuilder"
	"github.com/joeyloman/rancher-fip-api-server/pkg/types"
	fipv1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	managementv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getNextServiceID extracts the next available service ID from existing labels.
// It looks for keys matching the pattern "rancher.k8s.binbash.org/service-<id>-name"
// and returns the next available ID (maxID + 1). If no existing service IDs are found,
// it returns "0".
func getNextServiceID(labels map[string]string) string {
	maxID := -1
	for key := range labels {
		if strings.HasPrefix(key, "rancher.k8s.binbash.org/service-") && strings.HasSuffix(key, "-name") {
			// Extract the ID from the key
			prefix := "rancher.k8s.binbash.org/service-"
			suffix := "-name"
			idStr := strings.TrimPrefix(key, prefix)
			idStr = strings.TrimSuffix(idStr, suffix)
			id, err := strconv.Atoi(idStr)
			if err == nil && id > maxID {
				maxID = id
			}
		}
	}
	if maxID >= 0 {
		return strconv.Itoa(maxID + 1)
	}
	return "0"
}

// serviceAlreadyExists checks if a service with the given name and namespace
// already exists in the labels. It scans all service-<id>-name and
// service-<id>-namespace label pairs for a match.
func serviceAlreadyExists(labels map[string]string, serviceName, serviceNamespace string) bool {
	for key, val := range labels {
		if strings.HasPrefix(key, "rancher.k8s.binbash.org/service-") && strings.HasSuffix(key, "-name") {
			if val == serviceName {
				// Extract the service ID from the label key (e.g., "service-0-name" -> "0")
				serviceID := strings.TrimPrefix(strings.TrimSuffix(key, "-name"), "rancher.k8s.binbash.org/service-")
				namespaceKey := "rancher.k8s.binbash.org/service-" + serviceID + "-namespace"
				if nsVal, ok := labels[namespaceKey]; ok && nsVal == serviceNamespace {
					return true
				}
			}
		}
	}
	return false
}

var (
	// allocationWaitTimeout bounds how long FIPRequestHandler waits for the
	// manager controller to populate Status.IPAddr. A variable so tests can
	// shorten it.
	allocationWaitTimeout = 30 * time.Second

	// cleanupTimeout bounds post-watch cleanup deletes. The request context is
	// already expired/cancelled at that point, so cleanup runs on a fresh
	// bounded background context rather than an unbounded context.Background().
	cleanupTimeout = 10 * time.Second

	// fipUpdateBackoff bounds conflict retries for FloatingIP
	// read-modify-write updates; worst case ~1s, far below the request deadline.
	fipUpdateBackoff = wait.Backoff{
		Steps:    8,
		Duration: 10 * time.Millisecond,
		Factor:   1.5,
		Jitter:   0.1,
		Cap:      500 * time.Millisecond,
	}
)

// MaxRequestDuration is the worst-case wall-clock time a single request can
// hold its connection: FIPRequestHandler waits up to allocationWaitTimeout for
// the manager to populate Status.IPAddr, then runs its cleanup deletes on a
// fresh context bounded by cleanupTimeout. The HTTP server's graceful-shutdown
// grace must exceed this, otherwise a rolling restart kills the process while
// an allocation handler is still mid-write and resets the client instead of
// answering it.
func MaxRequestDuration() time.Duration {
	return allocationWaitTimeout + cleanupTimeout
}

// updateFloatingIP applies mutate to a freshly fetched copy of desired and
// retries on resourceVersion conflicts. Without the re-fetch, a concurrent
// writer (the manager controller or another request claiming the same FIP)
// would silently lose the labels this request just wrote, and a stale
// resourceVersion would surface as an opaque 500.
// On success desired reflects the persisted object.
func updateFloatingIP(ctx context.Context, cl client.Client, desired *fipv1beta2.FloatingIP, mutate func(*fipv1beta2.FloatingIP)) error {
	return updateFloatingIPIf(ctx, cl, desired, nil, mutate)
}

// updateFloatingIPIf is updateFloatingIP with a precondition: claimable runs
// against the freshly fetched object inside every retry attempt, and a false
// result aborts with errFIPClaimed instead of writing. Because the check sees
// the live object, a claim that landed between the caller's List and its
// Update is detected either by the fresh read or by the resourceVersion
// conflict the competing write caused. RetryOnConflict only retries Conflicts,
// so a claim refusal surfaces to the caller immediately.
func updateFloatingIPIf(ctx context.Context, cl client.Client, desired *fipv1beta2.FloatingIP, claimable func(*fipv1beta2.FloatingIP) bool, mutate func(*fipv1beta2.FloatingIP)) error {
	return retry.RetryOnConflict(fipUpdateBackoff, func() error {
		fresh := &fipv1beta2.FloatingIP{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: desired.Namespace, Name: desired.Name}, fresh); err != nil {
			return err
		}
		if claimable != nil && !claimable(fresh) {
			return errFIPClaimed
		}
		mutate(fresh)
		if err := cl.Update(ctx, fresh); err != nil {
			return err
		}
		*desired = *fresh
		return nil
	})
}

// errFIPClaimed marks a FloatingIP a concurrent request claimed first. It is
// deliberately not a Conflict: retrying the same object cannot help, the
// caller has to pick another candidate or create a new FloatingIP.
var errFIPClaimed = stderrors.New("floating ip already claimed by another request")

// fipClaimableByCluster reports whether a request pinned to cluster may write
// its service labels onto fip. A FIP without a cluster-name label is
// unclaimed; one claimed by another cluster is unavailable. The cluster-name
// label is written in the same update as the service labels, so it is visible
// immediately, whereas Status.Assigned only appears once the manager
// reconciles the claim.
func fipClaimableByCluster(fip *fipv1beta2.FloatingIP, cluster string) bool {
	claimedBy := fip.Labels["rancher.k8s.binbash.org/cluster-name"]
	return claimedBy == "" || claimedBy == cluster
}

// fipClaimableByRequest reports whether a request that did not pin an address
// may claim fip. An unclaimed FIP is always available; once claimed, the
// address is shared only with the same cluster through an explicit
// FloatingIPGroup, mirroring the group-reuse branch and the manager's group
// semantics.
func fipClaimableByRequest(fip *fipv1beta2.FloatingIP, cluster, group string) bool {
	claimedBy := fip.Labels["rancher.k8s.binbash.org/cluster-name"]
	if claimedBy == "" {
		return true
	}
	return group != "" && claimedBy == cluster && fip.Labels["rancher.k8s.binbash.org/floatingip-group"] == group
}

// applyFIPRequestLabels writes the project, cluster, service and group labels
// for a FIP request onto the object. It is idempotent: safe to call repeatedly
// on freshly fetched objects during conflict retry. It returns false when the
// service was already registered on the FIP.
func applyFIPRequestLabels(fip *fipv1beta2.FloatingIP, project, cluster, serviceName, serviceNamespace, group string) bool {
	if fip.Labels == nil {
		fip.Labels = make(map[string]string)
	}
	fip.Labels["rancher.k8s.binbash.org/project-name"] = project
	fip.Labels["rancher.k8s.binbash.org/cluster-name"] = cluster

	added := true
	if !serviceAlreadyExists(fip.Labels, serviceName, serviceNamespace) {
		serviceID := getNextServiceID(fip.Labels)
		fip.Labels["rancher.k8s.binbash.org/service-"+serviceID+"-name"] = serviceName
		fip.Labels["rancher.k8s.binbash.org/service-"+serviceID+"-namespace"] = serviceNamespace
	} else {
		added = false
	}

	if group != "" {
		fip.Labels["rancher.k8s.binbash.org/floatingip-group"] = group
	}
	return added
}

// writeUpdateError maps a FloatingIP update failure to a client-appropriate
// status: exhausted conflict retries are retryable (409), everything else 500.
func writeUpdateError(w http.ResponseWriter, err error) {
	if k8serrors.IsConflict(err) {
		errors.WriteJSONError(w, http.StatusConflict, "conflict updating floating IP, retry the request")
		return
	}
	errors.WriteJSONError(w, http.StatusInternalServerError, "failed to update floating IP")
}

// TokenHandler handles requests for authentication tokens.
// It generates a JWT signed with the application's private key.
func TokenHandler(app *types.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app.Log.Info("TokenHandler called")

		var request struct {
			ClientID string `json:"clientID"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			errors.WriteJSONError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// If the clientID is not provided, return an error
		if request.ClientID == "" {
			errors.WriteJSONError(w, http.StatusBadRequest, "clientID is required")
			return
		}

		token, expiresAt, err := auth.GenerateJWT(app.PrivateKey, request.ClientID, 1*time.Hour)
		if err != nil {
			app.Log.Errorf("failed to generate JWT: %s", err.Error())
			errors.WriteJSONError(w, http.StatusInternalServerError, "Failed to generate token")
			return
		}

		response := types.AuthTokenResponse{
			Token:     token,
			ExpiresAt: expiresAt,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}
}

// errHTTP is a handler failure that carries the HTTP status the client
// should see, so helpers can return a failure and the response layer stays in
// one place.
type errHTTP struct {
	status  int
	message string
}

func (e *errHTTP) Error() string { return e.message }

func newErrHTTP(status int, format string, args ...interface{}) *errHTTP {
	return &errHTTP{status: status, message: fmt.Sprintf(format, args...)}
}

// writeHandlerError answers w with err's status when it carries one and a
// generic 500 otherwise, so API internals never reach the client.
func writeHandlerError(w http.ResponseWriter, err error) {
	var he *errHTTP
	if stderrors.As(err, &he) {
		errors.WriteJSONError(w, he.status, he.message)
		return
	}
	errors.WriteJSONError(w, http.StatusInternalServerError, "failed to process floating IP request")
}

// createErrorToHTTP maps a FloatingIP create failure to a client-facing error:
// a 4xx StatusError keeps its code (a validating-webhook denial arrives as a
// 400 — k8s.io/apiserver's ToStatusErr clamps denial codes to >= 400 — and
// the webhook's message is client-facing), anything else becomes a sanitized
// 500 in writeHandlerError. The raw API error text used to be written into the
// body.
func createErrorToHTTP(err error) error {
	if k8serrors.IsAlreadyExists(err) {
		return newErrHTTP(http.StatusConflict, "floating IP request is already in progress, retry the request")
	}
	var status k8serrors.APIStatus
	if stderrors.As(err, &status) {
		s := status.Status()
		if s.Code >= http.StatusBadRequest && s.Code < http.StatusInternalServerError {
			message := s.Message
			if message == "" {
				message = "floating IP request rejected by the API server"
			}
			return newErrHTTP(int(s.Code), "%s", message)
		}
	}
	return err
}

// specificFIPName derives the deterministic object name of a pinned-address
// FloatingIP. The name is a pure function of the uniqueness domain the
// validating webhook enforces (pool + canonical address), so concurrent
// creates for one address collide on the API server's name uniqueness instead
// of persisting two FloatingIPs that claim it. Being server-side, it holds
// across apiserver replicas and restarts, unlike a per-process lock.
func specificFIPName(pool, ipAddr string) string {
	canonical := ipAddr
	if parsed := net.ParseIP(ipAddr); parsed != nil {
		canonical = parsed.String()
	}
	sum := sha256.Sum256([]byte(pool + "/" + canonical))
	return "fip-" + hex.EncodeToString(sum[:16])
}

// checkSpecificIPClaimable validates a pinned-address request against an
// existing FloatingIP before this request writes its labels onto it.
func checkSpecificIPClaimable(fip *fipv1beta2.FloatingIP, req types.FIPRequest) error {
	if fip.DeletionTimestamp != nil {
		// The address is released just before the finalizer is removed; a
		// retry lands on a fresh object instead of a disappearing one.
		return newErrHTTP(http.StatusConflict, "FloatingIP is being released, retry the request")
	}
	if fip.Labels["rancher.k8s.binbash.org/project-name"] != req.Project {
		// Only reachable through the create collision: the same pool and
		// address already belong to another project in this namespace.
		return newErrHTTP(http.StatusConflict, "IP address is already in use by another project")
	}
	if fip.Status.Assigned != nil {
		// Check if FloatingIPGroup is set in Status.Assigned
		if fip.Status.Assigned.FloatingIPGroup == "" {
			// IP is already assigned, which is an error condition
			return newErrHTTP(http.StatusConflict, "IP address is already assigned")
		}
		// FloatingIPGroup exists in Status.Assigned, check if request specifies a FloatingIPGroup
		if req.FloatingIPGroup != "" && req.FloatingIPGroup != fip.Status.Assigned.FloatingIPGroup {
			return newErrHTTP(http.StatusConflict, "FloatingIPGroup does not match the assigned FloatingIPGroup")
		}
		// Check if the existingFIP is already assigned to a different cluster-name
		if fip.Status.Assigned.ClusterName != req.Cluster {
			return newErrHTTP(http.StatusConflict, "FloatingIP is already in use by another cluster")
		}
	}
	return nil
}

// findPinnedFIP looks up the project's FloatingIP for a pinned address. The
// client is uncached, so the result reflects the API server's current state;
// callers re-run it after a failed create to pick up an object a concurrent
// request persisted.
func findPinnedFIP(ctx context.Context, app *types.App, project managementv3.Project, req types.FIPRequest) (*fipv1beta2.FloatingIP, bool, error) {
	fipList := &fipv1beta2.FloatingIPList{}
	if err := app.FipRestClient.List(ctx, fipList, client.InNamespace(project.Namespace)); err != nil {
		return nil, false, err
	}
	for i := range fipList.Items {
		// Check of the FloatingIP belongs to the project and equals the requested IP
		if fipList.Items[i].Labels["rancher.k8s.binbash.org/project-name"] == req.Project && fipList.Items[i].Spec.IPAddr != nil && *fipList.Items[i].Spec.IPAddr == req.IPAddress {
			return &fipList.Items[i], true, nil
		}
	}
	return nil, false, nil
}

// createSpecificIP persists the FloatingIP for a pinned address under its
// deterministic name. When a concurrent request got there first, the winner's
// object is adopted and returned with createdNew false, so the caller runs the
// normal claim checks against it and the allocation-timeout cleanup never
// deletes an object this request did not create.
func createSpecificIP(ctx context.Context, app *types.App, project managementv3.Project, req types.FIPRequest) (*fipv1beta2.FloatingIP, bool, error) {
	// Note: we allow to have FloatingIPGroups with the same name in different FloatingIPs

	fipLabels := make(map[string]string)
	fipLabels["rancher.k8s.binbash.org/project-name"] = project.Name
	fipLabels["rancher.k8s.binbash.org/cluster-name"] = req.Cluster
	fipLabels["rancher.k8s.binbash.org/service-0-name"] = req.ServiceName
	fipLabels["rancher.k8s.binbash.org/service-0-namespace"] = req.ServiceNamespace
	if req.FloatingIPGroup != "" {
		fipLabels["rancher.k8s.binbash.org/floatingip-group"] = req.FloatingIPGroup
	}

	fip := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      specificFIPName(req.FloatingIPPool, req.IPAddress),
			Namespace: project.Namespace,
			Labels:    fipLabels,
		},
		Spec: fipv1beta2.FloatingIPSpec{
			IPAddr:         &req.IPAddress,
			FloatingIPPool: req.FloatingIPPool,
		},
	}
	err := app.FipRestClient.Create(ctx, fip)
	if err == nil {
		app.Log.Infof("Created FloatingIP %s/%s", fip.Namespace, fip.Name)
		return fip, true, nil
	}

	if k8serrors.IsAlreadyExists(err) {
		// The deterministic name is taken: a concurrent request persisted this
		// address. Adopt that object instead of failing.
		existing := &fipv1beta2.FloatingIP{}
		if getErr := app.FipRestClient.Get(ctx, client.ObjectKey{Namespace: project.Namespace, Name: fip.Name}, existing); getErr == nil {
			if existing.Spec.IPAddr == nil || *existing.Spec.IPAddr != req.IPAddress || existing.Spec.FloatingIPPool != req.FloatingIPPool {
				// Defensive: the name is a hash, so a mismatch means the name
				// is taken by something this request must not touch.
				return nil, false, newErrHTTP(http.StatusConflict, "floating IP request conflicts with an existing object, retry the request")
			}
			return existing, false, nil
		} else if !k8serrors.IsNotFound(getErr) {
			app.Log.Errorf("failed to get floatingip %s/%s: %s", fip.Namespace, fip.Name, getErr.Error())
			return nil, false, newErrHTTP(http.StatusInternalServerError, "failed to read the existing floating IP")
		}
		// Released between the create and the get: fall through to the re-list.
	}

	// The create can also fail with the webhook's denial instead of
	// AlreadyExists: admission runs before persistence, so the loser of a
	// concurrent race is denied by the duplicate-IP reservation while the
	// winner's object is already persisted. Adopt it; only when the address is
	// genuinely still absent does the create error reach the client.
	if existing, found, listErr := findPinnedFIP(ctx, app, project, req); listErr != nil {
		app.Log.Errorf("failed to list floatingips after a create failure: %s", listErr.Error())
	} else if found {
		app.Log.Infof("Adopting FloatingIP %s/%s persisted by a concurrent request", existing.Namespace, existing.Name)
		return existing, false, nil
	}

	app.Log.Errorf("failed to create floatingip: %s", err.Error())
	return nil, false, createErrorToHTTP(err)
}

// requestSpecificIP allocates the FloatingIP for a request that pins an
// address, returning the object this request owns and whether it created it.
//
// Two concurrent requests for one address used to both List, both find
// nothing and both Create, leaving two FloatingIPs claiming the same address.
// The create now uses a deterministic name, and a failed
// create re-lists and adopts the winner's object, so the loser goes through
// the same claim checks instead of persisting a duplicate.
func requestSpecificIP(ctx context.Context, app *types.App, project managementv3.Project, req types.FIPRequest) (*fipv1beta2.FloatingIP, bool, error) {
	existing, found, err := findPinnedFIP(ctx, app, project, req)
	if err != nil {
		app.Log.Errorf("failed to list floatingips: %s", err.Error())
		return nil, false, newErrHTTP(http.StatusInternalServerError, "failed to list existing floating IPs")
	}

	if !found {
		created, createdNew, err := createSpecificIP(ctx, app, project, req)
		if err != nil {
			return nil, false, err
		}
		if createdNew {
			return created, true, nil
		}
		existing = created
	}

	if err := checkSpecificIPClaimable(existing, req); err != nil {
		return nil, false, err
	}

	// It exists but is not assigned (or is ours), so we can use it. The claim
	// is re-checked on the live object: a concurrent request may have claimed
	// this FIP after our List but before our Update, and its cluster-name
	// label is already visible even though the manager has not published
	// Status.Assigned yet.
	if err := updateFloatingIPIf(ctx, app.FipRestClient, existing, func(f *fipv1beta2.FloatingIP) bool {
		return fipClaimableByCluster(f, req.Cluster)
	}, func(f *fipv1beta2.FloatingIP) {
		if !applyFIPRequestLabels(f, project.Name, req.Cluster, req.ServiceName, req.ServiceNamespace, req.FloatingIPGroup) {
			app.Log.Errorf("Service %s/%s already exists in FloatingIP %s/%s, skipping duplicate label addition",
				req.ServiceNamespace, req.ServiceName, existing.Namespace, existing.Name)
		}
	}); err != nil {
		if stderrors.Is(err, errFIPClaimed) {
			return nil, false, newErrHTTP(http.StatusConflict, "FloatingIP is already in use by another cluster")
		}
		if k8serrors.IsConflict(err) {
			return nil, false, newErrHTTP(http.StatusConflict, "conflict updating floating IP, retry the request")
		}
		app.Log.Errorf("failed to update floatingip: %s", err.Error())
		return nil, false, err
	}
	app.Log.Infof("Updated existing FloatingIP %s/%s with new service labels", existing.Namespace, existing.Name)

	return existing, false, nil
}

// FIPRequestHandler handles requests to allocate a new floating IP.
// It creates a FloatingIP custom resource and waits for it to be reconciled.
func FIPRequestHandler(app *types.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app.Log.Info("FIPRequestHandler called")
		var fipRequest types.FIPRequest
		if err := json.NewDecoder(r.Body).Decode(&fipRequest); err != nil {
			errors.WriteJSONError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// // Find the project namespace
		var projects managementv3.ProjectList
		gvr := schema.GroupVersionResource{
			Group:    "management.cattle.io",
			Version:  "v3",
			Resource: "projects",
		}

		list, err := app.DynamicClient.Resource(gvr).List(r.Context(), metav1.ListOptions{})
		if err != nil {
			errors.WriteJSONError(w, http.StatusInternalServerError, "Failed to list projects")
			app.Log.Errorf("error gathering cluster list: %s", err.Error())
			return
		}

		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(list.UnstructuredContent(), &projects); err != nil {
			errors.WriteJSONError(w, http.StatusInternalServerError, "Failed to parse projects")
			app.Log.Errorf("error parsing cluster list: %s", err.Error())
			return
		}

		var project managementv3.Project
		projectFound := false
		for _, p := range projects.Items {
			if p.Name == fipRequest.Project {
				project = p
				projectFound = true
				break
			}
		}

		if !projectFound {
			errors.WriteJSONError(w, http.StatusBadRequest, "Project not found")
			app.Log.Errorf("project not found: %s", fipRequest.Project)
			return
		}

		// createdNew records whether this request created the FloatingIP
		// resource (versus reusing a pre-existing one). Only resources created
		// by this request may be cleaned up when the allocation watch fails.
		createdNew := false

		var floatingIP *fipv1beta2.FloatingIP
		if fipRequest.IPAddress != "" {
			// IP address is given in the request: look up, claim or create it.
			// The helper owns the create-collision handling so two requests
			// for one address cannot persist two FloatingIPs.
			floatingIP, createdNew, err = requestSpecificIP(r.Context(), app, project, fipRequest)
			if err != nil {
				app.Log.Errorf("failed to request ip %s: %s", fipRequest.IPAddress, err.Error())
				writeHandlerError(w, err)
				return
			}
		} else {
			// IP address is NOT given in the request

			// Check if there are unassigned floatingips in the project, if so use one of them
			fipList := &fipv1beta2.FloatingIPList{}
			if err := app.FipRestClient.List(r.Context(), fipList, client.InNamespace(project.Namespace)); err != nil {
				app.Log.Errorf("failed to list floatingips: %s", err.Error())
				errors.WriteJSONError(w, http.StatusInternalServerError, "failed to list existing floating IPs")
				return
			}

			var existingFIP *fipv1beta2.FloatingIP
			// First check if the fipRequest has a FloatingIPGroup set and search if the group already exists within the project and assigned to the cluster
			for i := range fipList.Items {
				if fipRequest.FloatingIPGroup != "" {
					if fipList.Items[i].Labels["rancher.k8s.binbash.org/project-name"] == fipRequest.Project && fipList.Items[i].Labels["rancher.k8s.binbash.org/cluster-name"] == fipRequest.Cluster && fipList.Items[i].Status.Assigned != nil && fipList.Items[i].Status.Assigned.FloatingIPGroup == fipRequest.FloatingIPGroup {
						// We found a FloatingIPGroup match within the project and assigned to the cluster so we need to use this fip
						existingFIP = &fipList.Items[i]
						break
					}
				}
			}

			claimLabels := func(f *fipv1beta2.FloatingIP) {
				if !applyFIPRequestLabels(f, project.Name, fipRequest.Cluster, fipRequest.ServiceName, fipRequest.ServiceNamespace, fipRequest.FloatingIPGroup) {
					app.Log.Errorf("Service %s/%s already exists in FloatingIP %s/%s, skipping duplicate label addition",
						fipRequest.ServiceNamespace, fipRequest.ServiceName, f.Namespace, f.Name)
				}
			}

			if existingFIP != nil {
				// The group match is already assigned to this cluster and group,
				// so sharing it is the requested behaviour.
				floatingIP = existingFIP
				if err := updateFloatingIP(r.Context(), app.FipRestClient, floatingIP, claimLabels); err != nil {
					app.Log.Errorf("failed to update floatingip: %s", err.Error())
					writeUpdateError(w, err)
					return
				}
				app.Log.Infof("Updated existing FloatingIP %s/%s with new service labels", floatingIP.Namespace, floatingIP.Name)
			} else {
				// No group match: claim an unassigned FloatingIP in the project.
				// The List snapshot cannot be trusted for exclusivity (a
				// concurrent request sees the same candidates), so each
				// candidate is claimed with the claim re-checked on the live
				// object. A candidate another request claimed first is skipped;
				// when none is left a new FloatingIP is created, exactly as if
				// the project had no unassigned FIP to begin with.
				for i := range fipList.Items {
					candidate := &fipList.Items[i]
					// Check of the FloatingIP belongs to the project and is not assigned
					if candidate.Labels["rancher.k8s.binbash.org/project-name"] != fipRequest.Project || candidate.Status.IPAddr == "" || candidate.Status.Assigned != nil {
						continue
					}
					err := updateFloatingIPIf(r.Context(), app.FipRestClient, candidate, func(f *fipv1beta2.FloatingIP) bool {
						return fipClaimableByRequest(f, fipRequest.Cluster, fipRequest.FloatingIPGroup)
					}, claimLabels)
					if stderrors.Is(err, errFIPClaimed) || k8serrors.IsNotFound(err) {
						// Claimed by a concurrent request, or gone since the
						// List: try the next candidate.
						app.Log.Infof("Skipping FloatingIP %s/%s: %s", candidate.Namespace, candidate.Name, err.Error())
						continue
					}
					if err != nil {
						app.Log.Errorf("failed to update floatingip: %s", err.Error())
						writeUpdateError(w, err)
						return
					}
					existingFIP = candidate
					break
				}

				if existingFIP != nil {
					floatingIP = existingFIP
					app.Log.Infof("Updated existing FloatingIP %s/%s with new service labels", floatingIP.Namespace, floatingIP.Name)
				} else {
					// No IP address requested, create a new one with the first service

					fipLabels := make(map[string]string)
					fipLabels["rancher.k8s.binbash.org/project-name"] = project.Name
					fipLabels["rancher.k8s.binbash.org/cluster-name"] = fipRequest.Cluster
					fipLabels["rancher.k8s.binbash.org/service-0-name"] = fipRequest.ServiceName
					fipLabels["rancher.k8s.binbash.org/service-0-namespace"] = fipRequest.ServiceNamespace
					if fipRequest.FloatingIPGroup != "" {
						fipLabels["rancher.k8s.binbash.org/floatingip-group"] = fipRequest.FloatingIPGroup
					}

					floatingIP = &fipv1beta2.FloatingIP{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: "fip-",
							Namespace:    project.Namespace,
							Labels:       fipLabels,
						},
						Spec: fipv1beta2.FloatingIPSpec{
							FloatingIPPool: fipRequest.FloatingIPPool,
						},
					}
					if err := app.FipRestClient.Create(r.Context(), floatingIP); err != nil {
						app.Log.Errorf("failed to create floatingip: %s", err.Error())
						writeHandlerError(w, createErrorToHTTP(err))
						return
					}
					createdNew = true
					app.Log.Infof("Created FloatingIP %s/%s", floatingIP.Namespace, floatingIP.Name)
				}
			}
		}

		// Watch for the IP address to be allocated
		ctx, cancel := context.WithTimeout(r.Context(), allocationWaitTimeout)
		defer cancel()

		finalFip, err := kubebuilder.WatchFloatingIP(ctx, app.FipWatchClient, floatingIP)
		if err != nil {
			app.Log.Errorf("error watching floating ip: %s", err.Error())

			switch {
			case stderrors.Is(err, context.DeadlineExceeded), stderrors.Is(err, context.Canceled):
				// Clean up only a FloatingIP this request created; a reused
				// pre-existing FIP belongs to the project and must survive a
				// controller backlog that pushes allocation past the deadline.
				if createdNew {
					cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanupTimeout)
					defer cleanupCancel()
					if delErr := app.FipRestClient.Delete(cleanupCtx, floatingIP); delErr != nil {
						app.Log.Errorf("failed to delete floatingip after watch ended: %s", delErr.Error())
					} else {
						app.Log.Infof("Deleted FloatingIP %s/%s after failed allocation", floatingIP.Namespace, floatingIP.Name)
					}
				} else {
					app.Log.Infof("Keeping pre-existing FloatingIP %s/%s after failed allocation", floatingIP.Namespace, floatingIP.Name)
				}
				if stderrors.Is(err, context.Canceled) {
					// Client disconnected; nothing to report to.
					return
				}
				errors.WriteJSONError(w, http.StatusGatewayTimeout, "timeout waiting for IP allocation")
				return
			default:
				errors.WriteJSONError(w, http.StatusInternalServerError, "failed to get ip address")
				return
			}
		}

		// Get the subnet from the floating IP pool
		fipPool := fipv1beta2.FloatingIPPool{}
		if err := app.FipRestClient.Get(r.Context(), client.ObjectKey{Name: fipRequest.FloatingIPPool}, &fipPool); err != nil {
			app.Log.Errorf("failed to get floatingippool subnet: %s", err.Error())
			errors.WriteJSONError(w, http.StatusInternalServerError, "failed to get floatingippool subnet")
			return
		}

		var fipGroup, sharedKey string
		if finalFip.Status.Assigned != nil {
			fipGroup = finalFip.Status.Assigned.FloatingIPGroup
			sharedKey = finalFip.Status.Assigned.SharedKey
		}

		subnet := ""
		if fipPool.Spec.IPConfig != nil {
			subnet = fipPool.Spec.IPConfig.Subnet
		} else {
			app.Log.Warnf("floatingippool %s has no ipConfig, returning empty subnet", fipRequest.FloatingIPPool)
		}

		fipResponse := types.FIPResponse{
			Status:           "approved",
			ClientSecret:     fipRequest.ClientSecret,
			Cluster:          fipRequest.Cluster,
			Project:          fipRequest.Project,
			FloatingIPPool:   fipRequest.FloatingIPPool,
			ServiceNamespace: fipRequest.ServiceNamespace,
			ServiceName:      fipRequest.ServiceName,
			IPAddress:        finalFip.Status.IPAddr,
			Subnet:           subnet,
			FloatingIPGroup:  fipGroup,
			SharedKey:        sharedKey,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(fipResponse)
	}
}

// FIPReleaseHandler handles requests to release a floating IP.
// It finds and deletes the corresponding FloatingIP custom resource.
func FIPReleaseHandler(app *types.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app.Log.Info("FIPReleaseHandler called")
		var fipReleaseRequest types.FIPReleaseRequest
		if err := json.NewDecoder(r.Body).Decode(&fipReleaseRequest); err != nil {
			errors.WriteJSONError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Find the project namespace
		var projects managementv3.ProjectList
		gvr := schema.GroupVersionResource{
			Group:    "management.cattle.io",
			Version:  "v3",
			Resource: "projects",
		}

		list, err := app.DynamicClient.Resource(gvr).List(r.Context(), metav1.ListOptions{})
		if err != nil {
			errors.WriteJSONError(w, http.StatusInternalServerError, "Failed to list projects")
			app.Log.Errorf("error gathering cluster list: %s", err.Error())
			return
		}

		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(list.UnstructuredContent(), &projects); err != nil {
			errors.WriteJSONError(w, http.StatusInternalServerError, "Failed to parse projects")
			app.Log.Errorf("error parsing cluster list: %s", err.Error())
			return
		}

		var project managementv3.Project
		projectFound := false
		for _, p := range projects.Items {
			if p.Name == fipReleaseRequest.Project {
				project = p
				projectFound = true
				break
			}
		}

		if !projectFound {
			errors.WriteJSONError(w, http.StatusBadRequest, "Project not found")
			app.Log.Errorf("project not found: %s", fipReleaseRequest.Project)
			return
		}

		// List floating IPs with a label selector for project and service
		fipList := &fipv1beta2.FloatingIPList{}
		err = app.FipRestClient.List(r.Context(), fipList, client.InNamespace(project.Namespace), client.MatchingLabels{
			"rancher.k8s.binbash.org/project-name": fipReleaseRequest.Project,
			"rancher.k8s.binbash.org/cluster-name": fipReleaseRequest.Cluster,
		})
		if err != nil {
			app.Log.Errorf("failed to list floatingips: %s", err.Error())
			errors.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}

		if len(fipList.Items) == 0 {
			app.Log.Warnf("no floatingip found for project %s in namespace %s to release for service %s/%s in cluster %s",
				fipReleaseRequest.Project, project.Namespace, fipReleaseRequest.ServiceNamespace, fipReleaseRequest.ServiceName, fipReleaseRequest.Cluster)
			errors.WriteJSONError(w, http.StatusNotFound, "no floatingip found to release")
			return
		}

		// Find the floating IP that contains the service to release
		var fipToUpdate fipv1beta2.FloatingIP
		found := false
		for i := range fipList.Items {
			fip := &fipList.Items[i]

			// Check if the floating IP has the requested IP address (if specified)
			if fipReleaseRequest.IPAddress != "" {
				if fip.Status.IPAddr != fipReleaseRequest.IPAddress {
					continue
				}
			}

			// If the FloatingIPGroup is given and we don't have a match we can skip this entry
			if fipReleaseRequest.FloatingIPGroup != "" {
				if fip.Labels["rancher.k8s.binbash.org/floatingip-group"] != fipReleaseRequest.FloatingIPGroup {
					continue
				}
			}

			// Look for service labels that match the requested service name and namespace
			for labelKey, labelValue := range fip.Labels {
				if strings.HasPrefix(labelKey, "rancher.k8s.binbash.org/service-") && strings.HasSuffix(labelKey, "-name") {
					// Extract the service ID from the label key (e.g., "service-0-name" -> "0")
					serviceID := strings.TrimPrefix(strings.TrimSuffix(labelKey, "-name"), "rancher.k8s.binbash.org/service-")
					namespaceKey := "rancher.k8s.binbash.org/service-" + serviceID + "-namespace"

					// Check if the namespace label exists and matches the requested service namespace
					if namespaceValue, ok := fip.Labels[namespaceKey]; ok {
						if namespaceValue == fipReleaseRequest.ServiceNamespace && labelValue == fipReleaseRequest.ServiceName {
							// Found the matching floating IP
							fipToUpdate = *fip
							found = true
							break
						}
					}
				}
			}
			if found {
				break
			}
		}

		if !found {
			app.Log.Warnf("no floatingip found for project %s in namespace %s to release for service %s/%s in cluster %s with IP %s",
				fipReleaseRequest.Project, project.Namespace, fipReleaseRequest.ServiceNamespace, fipReleaseRequest.ServiceName, fipReleaseRequest.Cluster, fipReleaseRequest.IPAddress)
			errors.WriteJSONError(w, http.StatusNotFound, "no floatingip found to release")
			return
		}

		// Find and delete the service labels that match the requested service
		// name and namespace, then drop the group/cluster labels that nothing
		// references anymore. The mutation is re-applied to a freshly fetched
		// copy on every conflict-retry attempt so concurrent writers cannot
		// make this update silently lose their changes.
		releaseMutate := func(fip *fipv1beta2.FloatingIP) {
			for labelKey, labelValue := range fip.Labels {
				if strings.HasPrefix(labelKey, "rancher.k8s.binbash.org/service-") && strings.HasSuffix(labelKey, "-name") {
					// Extract the service ID from the label key (e.g., "service-0-name" -> "0")
					serviceID := strings.TrimPrefix(strings.TrimSuffix(labelKey, "-name"), "rancher.k8s.binbash.org/service-")
					namespaceKey := "rancher.k8s.binbash.org/service-" + serviceID + "-namespace"

					// Check if the namespace label exists and matches the requested service namespace
					if namespaceValue, ok := fip.Labels[namespaceKey]; ok {
						if namespaceValue == fipReleaseRequest.ServiceNamespace && labelValue == fipReleaseRequest.ServiceName {
							// Found the matching service labels, delete them
							delete(fip.Labels, labelKey)
							delete(fip.Labels, namespaceKey)
							app.Log.Infof("Deleted service labels %s and %s for service %s/%s",
								labelKey, namespaceKey, fipReleaseRequest.ServiceNamespace, fipReleaseRequest.ServiceName)
							break
						}
					}
				}
			}

			// Check if this is the last service before removing the floatingip-group label
			if fip.Labels["rancher.k8s.binbash.org/floatingip-group"] != "" {
				// Count remaining service labels
				remainingServices := 0
				for labelKey := range fip.Labels {
					if strings.HasPrefix(labelKey, "rancher.k8s.binbash.org/service-") && strings.HasSuffix(labelKey, "-name") {
						remainingServices++
					}
				}
				// Only delete the floatingip-group label if no services remain
				if remainingServices == 0 {
					delete(fip.Labels, "rancher.k8s.binbash.org/floatingip-group")
				}
			}

			// If no service labels remain, drop the cluster-name label; the
			// status assigned and conditions are updated by the
			// rancher-fip-manager controller.
			hasServiceLabels := false
			for labelKey := range fip.Labels {
				if strings.HasPrefix(labelKey, "rancher.k8s.binbash.org/service-") && strings.HasSuffix(labelKey, "-name") {
					hasServiceLabels = true
					break
				}
			}
			if !hasServiceLabels {
				delete(fip.Labels, "rancher.k8s.binbash.org/cluster-name")
			}
		}

		if err := updateFloatingIP(r.Context(), app.FipRestClient, &fipToUpdate, releaseMutate); err != nil {
			app.Log.Errorf("failed to update floatingip: %s", err.Error())
			writeUpdateError(w, err)
			return
		}

		app.Log.Infof("Released FloatingIP %s in object %s/%s", fipReleaseRequest.IPAddress, fipToUpdate.Namespace, fipToUpdate.Name)

		fipReleaseResponse := types.FIPReleaseResponse{
			Status:  "released",
			Message: "Floating IP released successfully",
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(fipReleaseResponse)
	}
}

// FIPListHandler handles requests to list all floating IPs for a given project.
func FIPListHandler(app *types.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app.Log.Info("FIPListHandler called")
		var fipListRequest types.FIPListRequest
		if err := json.NewDecoder(r.Body).Decode(&fipListRequest); err != nil {
			errors.WriteJSONError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Find the project namespace
		var projects managementv3.ProjectList
		gvr := schema.GroupVersionResource{
			Group:    "management.cattle.io",
			Version:  "v3",
			Resource: "projects",
		}

		list, err := app.DynamicClient.Resource(gvr).List(r.Context(), metav1.ListOptions{})
		if err != nil {
			errors.WriteJSONError(w, http.StatusInternalServerError, "Failed to list projects")
			app.Log.Errorf("error gathering project list: %s", err.Error())
			return
		}

		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(list.UnstructuredContent(), &projects); err != nil {
			errors.WriteJSONError(w, http.StatusInternalServerError, "Failed to parse projects")
			app.Log.Errorf("error parsing project list: %s", err.Error())
			return
		}

		var project managementv3.Project
		projectFound := false
		for _, p := range projects.Items {
			if p.Name == fipListRequest.Project {
				project = p
				projectFound = true
				break
			}
		}

		if !projectFound {
			errors.WriteJSONError(w, http.StatusBadRequest, "Project not found")
			app.Log.Errorf("project not found: %s", fipListRequest.Project)
			return
		}

		// List all floating IPs for the project
		fipList := &fipv1beta2.FloatingIPList{}
		err = app.FipRestClient.List(r.Context(), fipList, client.InNamespace(project.Namespace), client.MatchingLabels{
			"rancher.k8s.binbash.org/project-name": fipListRequest.Project,
		})
		if err != nil {
			app.Log.Errorf("failed to list floatingips: %s", err.Error())
			errors.WriteJSONError(w, http.StatusInternalServerError, "failed to list floating IPs")
			return
		}

		var floatingIPs []types.FloatingIP
		for _, item := range fipList.Items {
			if item.Spec.FloatingIPPool == fipListRequest.FloatingIPPool {
				if item.Spec.IPAddr != nil {
					// Only return the floating IP if it is assigned to the cluster or if the cluster is not specified
					if item.Labels["rancher.k8s.binbash.org/cluster-name"] == fipListRequest.Cluster || item.Status.Assigned == nil {
						// Iterate through all service-<id>-name and service-<id>-namespace labels
						// and create a FloatingIP entry for each one
						serviceIDs := make(map[string]bool)
						for key := range item.Labels {
							if strings.HasPrefix(key, "rancher.k8s.binbash.org/service-") && strings.HasSuffix(key, "-name") {
								prefix := "rancher.k8s.binbash.org/service-"
								suffix := "-name"
								idStr := strings.TrimPrefix(key, prefix)
								idStr = strings.TrimSuffix(idStr, suffix)
								serviceIDs[idStr] = true
							}
						}

						// Create a FloatingIP for each service ID found
						for serviceID := range serviceIDs {
							fip := types.FloatingIP{
								Project:          item.Labels["rancher.k8s.binbash.org/project-name"],
								Cluster:          item.Labels["rancher.k8s.binbash.org/cluster-name"],
								FloatingIPPool:   item.Spec.FloatingIPPool,
								ServiceNamespace: item.Labels["rancher.k8s.binbash.org/service-"+serviceID+"-namespace"],
								ServiceName:      item.Labels["rancher.k8s.binbash.org/service-"+serviceID+"-name"],
								IPAddress:        *item.Spec.IPAddr,
							}
							if item.Labels["rancher.k8s.binbash.org/floatingip-group"] != "" {
								fip.FloatingIPGroup = item.Labels["rancher.k8s.binbash.org/floatingip-group"]
							}
							floatingIPs = append(floatingIPs, fip)
						}
					}
				}
			}
		}

		fipListResponse := types.FIPListResponse{
			ClientSecret: fipListRequest.ClientSecret,
			Cluster:      fipListRequest.Cluster,
			Project:      fipListRequest.Project,
			FloatingIPs:  floatingIPs,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(fipListResponse)
	}
}

// FIPDeleteHandler handles requests to delete a floating IP.
// It finds and deletes the corresponding FloatingIP custom resource.
func FIPDeleteHandler(app *types.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app.Log.Info("FIPDeleteHandler called")
		var fipDeleteRequest types.FIPDeleteRequest
		if err := json.NewDecoder(r.Body).Decode(&fipDeleteRequest); err != nil {
			errors.WriteJSONError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Find the project namespace
		var projects managementv3.ProjectList
		gvr := schema.GroupVersionResource{
			Group:    "management.cattle.io",
			Version:  "v3",
			Resource: "projects",
		}

		list, err := app.DynamicClient.Resource(gvr).List(r.Context(), metav1.ListOptions{})
		if err != nil {
			errors.WriteJSONError(w, http.StatusInternalServerError, "Failed to list projects")
			app.Log.Errorf("error gathering cluster list: %s", err.Error())
			return
		}

		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(list.UnstructuredContent(), &projects); err != nil {
			errors.WriteJSONError(w, http.StatusInternalServerError, "Failed to parse projects")
			app.Log.Errorf("error parsing cluster list: %s", err.Error())
			return
		}

		var project managementv3.Project
		projectFound := false
		for _, p := range projects.Items {
			if p.Name == fipDeleteRequest.Project {
				project = p
				projectFound = true
				break
			}
		}

		if !projectFound {
			errors.WriteJSONError(w, http.StatusBadRequest, "Project not found")
			app.Log.Errorf("project not found: %s", fipDeleteRequest.Project)
			return
		}

		// List floating IPs in the project's namespace
		fipList := &fipv1beta2.FloatingIPList{}
		err = app.FipRestClient.List(r.Context(), fipList, client.InNamespace(project.Namespace))
		if err != nil {
			app.Log.Errorf("failed to list floatingips: %s", err.Error())
			errors.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}

		var fipToDelete *fipv1beta2.FloatingIP
		for i, fip := range fipList.Items {
			if fip.Spec.IPAddr != nil && *fip.Spec.IPAddr == fipDeleteRequest.IPAddress {
				fipToDelete = &fipList.Items[i]
				break
			}
		}

		if fipToDelete == nil {
			app.Log.Warnf("no floatingip found with address %s for project %s in namespace %s",
				fipDeleteRequest.IPAddress, fipDeleteRequest.Project, project.Namespace)
			errors.WriteJSONError(w, http.StatusNotFound, "no floatingip found to delete")
			return
		}

		err = app.FipRestClient.Delete(r.Context(), fipToDelete)
		if err != nil {
			app.Log.Errorf("failed to delete floatingip: %s", err.Error())
			errors.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}

		app.Log.Infof("Deleted FloatingIP %s/%s with IP %s", fipToDelete.Namespace, fipToDelete.Name, fipDeleteRequest.IPAddress)

		fipDeleteResponse := types.FIPDeleteResponse{
			Status:  "deleted",
			Message: "Floating IP deleted successfully",
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(fipDeleteResponse)
	}
}
