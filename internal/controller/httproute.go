package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	finalizerName              = "gateway-auto-listener/finalizer"
	clusterIssuerAnnotation    = "cert-manager.io/cluster-issuer"
	issuerAnnotation           = "cert-manager.io/issuer"
	managedByLabel             = "gateway-auto-listener/managed-by"
	managedByValue             = "gateway-auto-listener"
	managedHostnamesAnnotation = "gateway-auto-listener/managed-hostnames" // legacy: read for migration only
	// ownerAnnotationPrefix is applied to the Gateway as `<prefix><listenerName>` and
	// records "<routeNamespace>/<routeName>" as the canonical owner of that listener.
	// Replaces the route-local managed-hostnames annotation, which was tenant-writable
	// and could be exploited to drive cross-tenant listener deletion or rewriting.
	ownerAnnotationPrefix = "gateway-auto-listener.itsh.dev/owner."
)

type HTTPRouteReconciler struct {
	client.Client
	Scheme                     *runtime.Scheme
	Recorder                   record.EventRecorder
	GatewayName                string
	GatewayNamespace           string
	AllowedDomainSuffix        string
	ValidatedNSPrefix          string
	AllowedHostnamesAnnotation string
}

func (r *HTTPRouteReconciler) hasCertAnnotation(httpRoute *gatewayv1.HTTPRoute) bool {
	if _, ok := httpRoute.Annotations[clusterIssuerAnnotation]; ok {
		return true
	}
	if _, ok := httpRoute.Annotations[issuerAnnotation]; ok {
		return true
	}
	return false
}

func (r *HTTPRouteReconciler) validateHostname(ctx context.Context, hostname, namespace string) error {
	if r.ValidatedNSPrefix == "" {
		return nil
	}

	if !strings.HasPrefix(namespace, r.ValidatedNSPrefix) {
		return nil
	}

	if r.AllowedDomainSuffix != "" {
		defaultSuffix := fmt.Sprintf(".%s.%s", namespace, r.AllowedDomainSuffix)
		if strings.HasSuffix(hostname, defaultSuffix) {
			return nil
		}
	}

	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		return fmt.Errorf("failed to get namespace: %w", err)
	}

	if r.AllowedHostnamesAnnotation != "" {
		allowedHostnames := ns.Annotations[r.AllowedHostnamesAnnotation]
		if allowedHostnames != "" {
			for _, allowed := range strings.Split(allowedHostnames, ",") {
				allowed = strings.TrimSpace(allowed)
				if hostname == allowed || strings.HasSuffix(hostname, "."+allowed) {
					return nil
				}
			}
		}
	}

	return fmt.Errorf("hostname %s not allowed for namespace %s", hostname, namespace)
}

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var httpRoute gatewayv1.HTTPRoute
	if err := r.Get(ctx, req.NamespacedName, &httpRoute); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !r.hasCertAnnotation(&httpRoute) {
		return ctrl.Result{}, nil
	}

	// Skip the ParentRefs check on the deletion path: a tenant might have edited the
	// route to drop its parentRef before deleting it, and we still need the finalizer
	// to clean up listeners we own. Ownership is checked inside removeListeners.
	if httpRoute.DeletionTimestamp.IsZero() && !r.routeReferencesManagedGateway(&httpRoute) {
		// Route doesn't list our Gateway as a parent; nothing to do.
		// Prevents creating phantom listeners for routes attached to a different Gateway.
		return ctrl.Result{}, nil
	}

	// Handle deletion
	if !httpRoute.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&httpRoute, finalizerName) {
			if err := r.removeListeners(ctx, &httpRoute); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&httpRoute, finalizerName)
			if err := r.Update(ctx, &httpRoute); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&httpRoute, finalizerName) {
		controllerutil.AddFinalizer(&httpRoute, finalizerName)
		if err := r.Update(ctx, &httpRoute); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.reconcileListeners(ctx, &httpRoute, httpRoute.Namespace); err != nil {
		log.Error(err, "failed to reconcile listeners")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *HTTPRouteReconciler) reconcileListeners(ctx context.Context, httpRoute *gatewayv1.HTTPRoute, routeNamespace string) error {
	log := log.FromContext(ctx)

	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{
		Name:      r.GatewayName,
		Namespace: r.GatewayNamespace,
	}, &gateway); err != nil {
		return fmt.Errorf("failed to get gateway: %w", err)
	}

	existingListeners := make(map[string]bool)
	for _, l := range gateway.Spec.Listeners {
		existingListeners[string(l.Name)] = true
	}

	ownerKey := routeOwnerKey(httpRoute.Namespace, httpRoute.Name)

	// Build set of currently-desired listener names (only validated hostnames).
	currentListeners := make(map[string]bool)
	for _, hostname := range httpRoute.Spec.Hostnames {
		if err := r.validateHostname(ctx, string(hostname), httpRoute.Namespace); err != nil {
			log.Error(err, "hostname validation failed", "hostname", hostname)
			r.Recorder.Eventf(httpRoute, corev1.EventTypeWarning, "HostnameValidationFailed",
				"hostname %s not allowed for namespace %s", string(hostname), httpRoute.Namespace)
			continue
		}
		currentListeners[hostnameToListenerName(string(hostname))] = true
	}

	// previousListeners are ones the Gateway records as owned by THIS route via its annotations.
	// Authoritative source — never trusts annotations on the (tenant-writable) HTTPRoute.
	previousListeners := listenersOwnedBy(&gateway, ownerKey)

	// One-shot legacy migration: if the Gateway has no owner annotation for THIS route AND
	// the route still carries the legacy managed-hostnames annotation, claim listener names
	// from that annotation. Two safety gates prevent the original cross-tenant-deletion
	// attack from being re-introduced through this fallback:
	//   1. The listener must not already be owned by a different route.
	//   2. The listener's existing hostname must pass validateHostname() for THIS route's
	//      namespace. During the v0.1.x → v0.2.0 upgrade window, legitimate listeners have
	//      no owner annotations yet, so check (1) alone is insufficient — without (2) an
	//      attacker tenant could race in with a forged legacy annotation and claim
	//      victim.com because no owner exists yet. validateHostname will reject victim.com
	//      for tenant-attacker namespace.
	if len(previousListeners) == 0 {
		if prev := httpRoute.Annotations[managedHostnamesAnnotation]; prev != "" {
			listenerByName := make(map[string]*gatewayv1.Listener, len(gateway.Spec.Listeners))
			for i := range gateway.Spec.Listeners {
				l := &gateway.Spec.Listeners[i]
				listenerByName[string(l.Name)] = l
			}
			for _, name := range strings.Split(prev, ",") {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				if owner, owned := gateway.Annotations[ownerAnnotationPrefix+name]; owned && owner != ownerKey {
					log.Info("ignoring legacy managed-hostnames entry: listener owned by another route",
						"listener", name, "current_owner", owner, "claimant", ownerKey)
					continue
				}
				if l, exists := listenerByName[name]; exists && l.Hostname != nil {
					if err := r.validateHostname(ctx, string(*l.Hostname), httpRoute.Namespace); err != nil {
						log.Info("ignoring legacy managed-hostnames entry: hostname not allowed for namespace",
							"listener", name, "hostname", *l.Hostname, "namespace", httpRoute.Namespace, "reason", err)
						continue
					}
				}
				previousListeners[name] = true
			}
		}
	}

	gwPatch := client.MergeFrom(gateway.DeepCopy())
	var newGWListeners []gatewayv1.Listener
	var removed, added int

	// Pass 1: walk existing listeners. Drop those owned by this route that are no longer desired.
	// Listeners owned by other routes (or unowned) are preserved unchanged.
	for _, l := range gateway.Spec.Listeners {
		name := string(l.Name)
		if previousListeners[name] && !currentListeners[name] {
			log.Info("removing stale listener", "listener", name)
			removed++
			continue
		}
		newGWListeners = append(newGWListeners, l)
	}

	// Pass 2: add new listeners for desired hostnames not already present on the Gateway.
	allowFrom := gatewayv1.NamespacesFromSelector
	for _, hostname := range httpRoute.Spec.Hostnames {
		if err := r.validateHostname(ctx, string(hostname), httpRoute.Namespace); err != nil {
			continue // already logged above
		}
		listenerName := hostnameToListenerName(string(hostname))
		if existingListeners[listenerName] {
			// Listener exists. If owned by another route, leave it alone (cross-tenant safety).
			// If owned by this route, currentListeners already kept it via Pass 1.
			// If unowned (legacy), Pass 3 will claim ownership without modifying spec.
			continue
		}

		secretName := hostnameToSecretName(string(hostname))
		ns := gatewayv1.Namespace(r.GatewayNamespace)
		hostnameVal := gatewayv1.Hostname(hostname)
		tlsMode := gatewayv1.TLSModeTerminate
		listener := gatewayv1.Listener{
			Name:     gatewayv1.SectionName(listenerName),
			Hostname: &hostnameVal,
			Port:     443,
			Protocol: gatewayv1.HTTPSProtocolType,
			AllowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &allowFrom,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": routeNamespace,
						},
					},
				},
			},
			TLS: &gatewayv1.ListenerTLSConfig{
				Mode: &tlsMode,
				CertificateRefs: []gatewayv1.SecretObjectReference{
					{
						Name:      gatewayv1.ObjectName(secretName),
						Namespace: &ns,
					},
				},
			},
		}
		newGWListeners = append(newGWListeners, listener)
		added++
		log.Info("adding listener", "listener", listenerName, "hostname", hostname, "secret", secretName)
	}

	// Pass 3: reconcile owner annotations on the Gateway.
	// Claim listeners we now own (currentListeners ∪ unowned-but-matching), release listeners
	// we previously owned but no longer desire.
	annotationsChanged := updateOwnerAnnotations(&gateway, ownerKey, currentListeners, previousListeners)

	if added > 0 || removed > 0 || annotationsChanged {
		gateway.Spec.Listeners = newGWListeners
		if gateway.Labels == nil {
			gateway.Labels = make(map[string]string)
		}
		gateway.Labels[managedByLabel] = managedByValue
		if err := r.Patch(ctx, &gateway, gwPatch); err != nil {
			return fmt.Errorf("failed to patch gateway: %w", err)
		}
	}

	// Strip the legacy managed-hostnames annotation off the route once we've migrated.
	// Removing it eliminates the tenant-writable surface that drove the original vulnerability.
	if _, ok := httpRoute.Annotations[managedHostnamesAnnotation]; ok {
		delete(httpRoute.Annotations, managedHostnamesAnnotation)
		if err := r.Update(ctx, httpRoute); err != nil {
			return fmt.Errorf("failed to remove legacy managed-hostnames annotation: %w", err)
		}
	}

	return nil
}

// routeOwnerKey returns the canonical "namespace/name" string used as ownership annotation value.
func routeOwnerKey(namespace, name string) string {
	return namespace + "/" + name
}

// listenersOwnedBy returns the set of listener names whose owner annotation matches ownerKey.
func listenersOwnedBy(gw *gatewayv1.Gateway, ownerKey string) map[string]bool {
	out := make(map[string]bool)
	for k, v := range gw.Annotations {
		if !strings.HasPrefix(k, ownerAnnotationPrefix) {
			continue
		}
		if v == ownerKey {
			out[strings.TrimPrefix(k, ownerAnnotationPrefix)] = true
		}
	}
	return out
}

// updateOwnerAnnotations adjusts the Gateway's owner annotations so the union of
// (currentListeners) is claimed for ownerKey, and any listener formerly owned by ownerKey
// but no longer in currentListeners is released. Returns true if annotations changed.
func updateOwnerAnnotations(gw *gatewayv1.Gateway, ownerKey string, currentListeners, previousListeners map[string]bool) bool {
	if gw.Annotations == nil {
		gw.Annotations = make(map[string]string)
	}
	changed := false

	for name := range currentListeners {
		key := ownerAnnotationPrefix + name
		existing, present := gw.Annotations[key]
		if !present {
			gw.Annotations[key] = ownerKey
			changed = true
			continue
		}
		if existing != ownerKey {
			// Owned by another route. Don't steal — current behavior is "first owner wins".
			continue
		}
	}

	for name := range previousListeners {
		if currentListeners[name] {
			continue
		}
		key := ownerAnnotationPrefix + name
		if existing, present := gw.Annotations[key]; present && existing == ownerKey {
			delete(gw.Annotations, key)
			changed = true
		}
	}

	return changed
}

// routeReferencesManagedGateway returns true if the HTTPRoute has at least one ParentRef
// pointing at the Gateway this controller manages. Routes attached to other Gateways are
// ignored to prevent the controller from creating phantom listeners on this Gateway based
// solely on a cert-manager annotation.
func (r *HTTPRouteReconciler) routeReferencesManagedGateway(httpRoute *gatewayv1.HTTPRoute) bool {
	for _, p := range httpRoute.Spec.ParentRefs {
		if string(p.Name) != r.GatewayName {
			continue
		}
		// Default to route's namespace when ParentRef.Namespace is unset.
		ns := httpRoute.Namespace
		if p.Namespace != nil && *p.Namespace != "" {
			ns = string(*p.Namespace)
		}
		if ns != r.GatewayNamespace {
			continue
		}
		return true
	}
	return false
}

func (r *HTTPRouteReconciler) removeListeners(ctx context.Context, httpRoute *gatewayv1.HTTPRoute) error {
	log := log.FromContext(ctx)

	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{
		Name:      r.GatewayName,
		Namespace: r.GatewayNamespace,
	}, &gateway); err != nil {
		return client.IgnoreNotFound(err)
	}

	// Only remove listeners we actually own — derive the set from the Gateway's owner
	// annotations, never from tenant-writable fields on the HTTPRoute. The route's
	// Spec.Hostnames and the legacy managed-hostnames annotation are tenant-controllable
	// and were the original cross-tenant deletion vector.
	ownerKey := routeOwnerKey(httpRoute.Namespace, httpRoute.Name)
	listenersToRemove := listenersOwnedBy(&gateway, ownerKey)
	if len(listenersToRemove) == 0 {
		return nil
	}

	patch := client.MergeFrom(gateway.DeepCopy())

	var newListeners []gatewayv1.Listener
	for _, l := range gateway.Spec.Listeners {
		if listenersToRemove[string(l.Name)] {
			log.Info("removing listener", "listener", l.Name)
			continue
		}
		newListeners = append(newListeners, l)
	}

	// Release the owner annotations alongside listener removal. Do this before the
	// listener-count guard so orphan annotations (listeners removed externally) get
	// cleaned up too.
	annotationsRemoved := false
	for name := range listenersToRemove {
		if _, present := gateway.Annotations[ownerAnnotationPrefix+name]; present {
			delete(gateway.Annotations, ownerAnnotationPrefix+name)
			annotationsRemoved = true
		}
	}

	listenersChanged := len(newListeners) != len(gateway.Spec.Listeners)
	if !listenersChanged && !annotationsRemoved {
		return nil
	}
	if listenersChanged {
		gateway.Spec.Listeners = newListeners
	}
	if err := r.Patch(ctx, &gateway, patch); err != nil {
		return fmt.Errorf("failed to patch gateway: %w", err)
	}

	return nil
}

func hostnameToListenerName(hostname string) string {
	sanitized := strings.ReplaceAll(hostname, ".", "-")
	sanitized = strings.ReplaceAll(sanitized, "*", "wildcard")
	return fmt.Sprintf("https-%s", sanitized)
}

func hostnameToSecretName(hostname string) string {
	sanitized := strings.ReplaceAll(hostname, ".", "-")
	sanitized = strings.ReplaceAll(sanitized, "*", "wildcard")
	return fmt.Sprintf("%s-tls", sanitized)
}

func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Watches(&gatewayv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.gatewayToHTTPRoutes)).
		Watches(&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceToHTTPRoutes),
			builder.WithPredicates(r.namespaceAnnotationChanged()),
		).
		Complete(r)
}

// namespaceToHTTPRoutes maps a Namespace event to all managed HTTPRoutes in that namespace,
// enabling re-reconciliation when the allowed-hostnames annotation changes.
func (r *HTTPRouteReconciler) namespaceToHTTPRoutes(ctx context.Context, obj client.Object) []reconcile.Request {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return nil
	}

	if r.ValidatedNSPrefix != "" && !strings.HasPrefix(ns.Name, r.ValidatedNSPrefix) {
		return nil
	}

	var httpRouteList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &httpRouteList, client.InNamespace(ns.Name)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, route := range httpRouteList.Items {
		if !r.hasCertAnnotation(&route) {
			continue
		}
		if !controllerutil.ContainsFinalizer(&route, finalizerName) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      route.Name,
				Namespace: route.Namespace,
			},
		})
	}
	return requests
}

func (r *HTTPRouteReconciler) namespaceAnnotationChanged() predicate.Predicate {
	annotation := r.AllowedHostnamesAnnotation
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			if annotation == "" {
				return false
			}
			_, exists := e.Object.GetAnnotations()[annotation]
			return exists
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if annotation == "" {
				return false
			}
			oldVal := e.ObjectOld.GetAnnotations()[annotation]
			newVal := e.ObjectNew.GetAnnotations()[annotation]
			return oldVal != newVal
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}
}

// gatewayToHTTPRoutes maps a Gateway event back to all HTTPRoutes that reference it,
// enabling re-reconciliation when a managed listener is manually deleted.
func (r *HTTPRouteReconciler) gatewayToHTTPRoutes(ctx context.Context, obj client.Object) []reconcile.Request {
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}

	if gateway.Name != r.GatewayName || gateway.Namespace != r.GatewayNamespace {
		return nil
	}

	var httpRouteList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &httpRouteList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, route := range httpRouteList.Items {
		if !r.hasCertAnnotation(&route) {
			continue
		}
		if !controllerutil.ContainsFinalizer(&route, finalizerName) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      route.Name,
				Namespace: route.Namespace,
			},
		})
	}
	return requests
}
