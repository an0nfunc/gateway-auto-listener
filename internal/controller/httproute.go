package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	finalizerName           = "gateway-auto-listener/finalizer"
	oldFinalizerName        = "httproute-cert-controller.itsh.dev/finalizer"
	clusterIssuerAnnotation = "cert-manager.io/cluster-issuer"
	issuerAnnotation        = "cert-manager.io/issuer"
	managedByLabel          = "gateway-auto-listener/managed-by"
	managedByValue          = "gateway-auto-listener"
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

	// Migrate old finalizer to new one
	if controllerutil.ContainsFinalizer(&httpRoute, oldFinalizerName) {
		controllerutil.RemoveFinalizer(&httpRoute, oldFinalizerName)
		controllerutil.AddFinalizer(&httpRoute, finalizerName)
		if err := r.Update(ctx, &httpRoute); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("migrated finalizer from old name to new name")
	}

	if !r.hasCertAnnotation(&httpRoute) {
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

	if err := r.ensureListeners(ctx, &httpRoute); err != nil {
		log.Error(err, "failed to ensure listeners")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *HTTPRouteReconciler) ensureListeners(ctx context.Context, httpRoute *gatewayv1.HTTPRoute) error {
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

	patch := client.MergeFrom(gateway.DeepCopy())

	var added int
	for _, hostname := range httpRoute.Spec.Hostnames {
		if err := r.validateHostname(ctx, string(hostname), httpRoute.Namespace); err != nil {
			log.Error(err, "hostname validation failed", "hostname", hostname)
			r.Recorder.Eventf(httpRoute, corev1.EventTypeWarning, "HostnameValidationFailed",
				"hostname %s not allowed for namespace %s", string(hostname), httpRoute.Namespace)
			continue
		}

		listenerName := hostnameToListenerName(string(hostname))
		if existingListeners[listenerName] {
			log.V(1).Info("listener already exists", "listener", listenerName)
			continue
		}

		secretName := hostnameToSecretName(string(hostname))
		ns := gatewayv1.Namespace(r.GatewayNamespace)
		hostnameVal := gatewayv1.Hostname(hostname)
		tlsMode := gatewayv1.TLSModeTerminate
		allowAll := gatewayv1.NamespacesFromAll

		listener := gatewayv1.Listener{
			Name:     gatewayv1.SectionName(listenerName),
			Hostname: &hostnameVal,
			Port:     443,
			Protocol: gatewayv1.HTTPSProtocolType,
			AllowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &allowAll,
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
		gateway.Spec.Listeners = append(gateway.Spec.Listeners, listener)
		added++
		log.Info("adding listener", "listener", listenerName, "hostname", hostname, "secret", secretName)
	}

	if added == 0 {
		return nil
	}

	// Label the gateway to indicate it's managed
	if gateway.Labels == nil {
		gateway.Labels = make(map[string]string)
	}
	gateway.Labels[managedByLabel] = managedByValue

	if err := r.Patch(ctx, &gateway, patch); err != nil {
		return fmt.Errorf("failed to patch gateway: %w", err)
	}

	return nil
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

	listenersToRemove := make(map[string]bool)
	for _, hostname := range httpRoute.Spec.Hostnames {
		listenerName := hostnameToListenerName(string(hostname))
		listenersToRemove[listenerName] = true
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

	if len(newListeners) == len(gateway.Spec.Listeners) {
		return nil
	}

	gateway.Spec.Listeners = newListeners
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
		Complete(r)
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
