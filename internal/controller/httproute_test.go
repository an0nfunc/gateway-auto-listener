package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// fakeEventRecorder satisfies the events.EventRecorder interface for tests.
// k8s.io/client-go/tools/events doesn't ship a stub equivalent to
// record.NewFakeRecorder, so we provide a minimal channel-based one.
type fakeEventRecorder struct {
	Events chan string
}

func newFakeEventRecorder(buf int) *fakeEventRecorder {
	return &fakeEventRecorder{Events: make(chan string, buf)}
}

func (f *fakeEventRecorder) Eventf(_, _ runtime.Object, eventtype, reason, action, note string, args ...interface{}) {
	msg := fmt.Sprintf("%s %s %s: "+note, append([]interface{}{eventtype, reason, action}, args...)...)
	select {
	case f.Events <- msg:
	default:
	}
}

func init() {
	_ = gatewayv1.Install(scheme.Scheme)
}

func TestHostnameToListenerName(t *testing.T) {
	tests := []struct {
		hostname string
		expected string
	}{
		{"example.com", "https-example-com"},
		{"sub.example.com", "https-sub-example-com"},
		{"*.example.com", "https-wildcard-example-com"},
		{"a.b.c.d.example.com", "https-a-b-c-d-example-com"},
		{"example", "https-example"},
		{"", "https-"},
	}

	for _, tt := range tests {
		t.Run(tt.hostname, func(t *testing.T) {
			result := hostnameToListenerName(tt.hostname)
			if result != tt.expected {
				t.Errorf("hostnameToListenerName(%q) = %q, want %q", tt.hostname, result, tt.expected)
			}
		})
	}
}

func TestHostnameToSecretName(t *testing.T) {
	tests := []struct {
		hostname string
		expected string
	}{
		{"example.com", "example-com-tls"},
		{"sub.example.com", "sub-example-com-tls"},
		{"*.example.com", "wildcard-example-com-tls"},
		{"a.b.c.d.example.com", "a-b-c-d-example-com-tls"},
		{"example", "example-tls"},
		{"", "-tls"},
	}

	for _, tt := range tests {
		t.Run(tt.hostname, func(t *testing.T) {
			result := hostnameToSecretName(tt.hostname)
			if result != tt.expected {
				t.Errorf("hostnameToSecretName(%q) = %q, want %q", tt.hostname, result, tt.expected)
			}
		})
	}
}

// parentRefsToDefault returns ParentRefs pointing at the managed Gateway used in test setups.
// Tests must include this so the reconciler's ParentRefs check accepts the route.
func parentRefsToDefault() []gatewayv1.ParentReference {
	ns := gatewayv1.Namespace("nginx-gateway")
	return []gatewayv1.ParentReference{
		{Name: gatewayv1.ObjectName("default"), Namespace: &ns},
	}
}

func newReconciler(objs ...client.Object) *HTTPRouteReconciler {
	cb := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...)
	cb = cb.WithStatusSubresource(objs...)

	return &HTTPRouteReconciler{
		Client:                     cb.Build(),
		Scheme:                     scheme.Scheme,
		Recorder:                   newFakeEventRecorder(10),
		GatewayName:                "default",
		GatewayNamespace:           "nginx-gateway",
		AllowedDomainSuffix:        "example.com",
		ValidatedNSPrefix:          "tenant-",
		AllowedHostnamesAnnotation: "gateway-auto-listener/allowed-hostnames",
	}
}

func TestValidateHostname_PlatformNamespace(t *testing.T) {
	r := newReconciler()
	ctx := context.Background()

	err := r.validateHostname(ctx, "anything.example.com", "nginx-gateway")
	if err != nil {
		t.Errorf("platform namespace should allow any hostname, got: %v", err)
	}
}

func TestValidateHostname_ValidatedNSPrefix_Disabled(t *testing.T) {
	r := newReconciler()
	r.ValidatedNSPrefix = ""
	ctx := context.Background()

	err := r.validateHostname(ctx, "evil.example.com", "tenant-123")
	if err != nil {
		t.Errorf("empty validated-ns-prefix should disable validation, got: %v", err)
	}
}

func TestValidateHostname_TenantDefaultSuffix(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-123"}}
	r := newReconciler(ns)
	ctx := context.Background()

	// Allowed: matches <hostname>.<namespace>.<suffix>
	err := r.validateHostname(ctx, "app.tenant-123.example.com", "tenant-123")
	if err != nil {
		t.Errorf("default suffix hostname should be allowed, got: %v", err)
	}

	// Disallowed: doesn't match
	err = r.validateHostname(ctx, "evil.other.com", "tenant-123")
	if err == nil {
		t.Error("non-matching hostname should be rejected")
	}
}

func TestValidateHostname_CustomDomains(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tenant-456",
			Annotations: map[string]string{
				"gateway-auto-listener/allowed-hostnames": "custom.org, another.net",
			},
		},
	}
	r := newReconciler(ns)
	ctx := context.Background()

	// Exact match
	err := r.validateHostname(ctx, "custom.org", "tenant-456")
	if err != nil {
		t.Errorf("exact match custom domain should be allowed, got: %v", err)
	}

	// Subdomain match
	err = r.validateHostname(ctx, "sub.custom.org", "tenant-456")
	if err != nil {
		t.Errorf("subdomain of custom domain should be allowed, got: %v", err)
	}

	// Subdomain of second entry
	err = r.validateHostname(ctx, "test.another.net", "tenant-456")
	if err != nil {
		t.Errorf("subdomain of allowed domain should be allowed, got: %v", err)
	}

	// Not allowed
	err = r.validateHostname(ctx, "evil.example.com", "tenant-456")
	if err == nil {
		t.Error("non-matching hostname should be rejected")
	}
}

func TestValidateHostname_EmptyAllowedDomainSuffix(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-789"}}
	r := newReconciler(ns)
	r.AllowedDomainSuffix = ""
	ctx := context.Background()

	// Without domain suffix, only annotation-based validation applies
	err := r.validateHostname(ctx, "app.tenant-789.example.com", "tenant-789")
	if err == nil {
		t.Error("without domain suffix and no annotation, hostname should be rejected")
	}
}

func TestReconcile_SkipWithoutAnnotation(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners:        []gatewayv1.Listener{},
		},
	}
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"test.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(gateway, httpRoute)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("should not requeue")
	}

	// Gateway should have no new listeners
	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)
	if len(gw.Spec.Listeners) != 0 {
		t.Errorf("expected 0 listeners, got %d", len(gw.Spec.Listeners))
	}
}

func TestReconcile_CreatesListener(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners:        []gatewayv1.Listener{},
		},
	}
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer": "letsencrypt",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"test.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(gateway, httpRoute)
	ctx := context.Background()

	// First reconcile: add finalizer
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Second reconcile: create listener
	_, err = r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)

	if len(gw.Spec.Listeners) != 1 {
		t.Fatalf("expected 1 listener, got %d", len(gw.Spec.Listeners))
	}

	listener := gw.Spec.Listeners[0]
	if string(listener.Name) != "https-test-example-com" {
		t.Errorf("expected listener name 'https-test-example-com', got %q", listener.Name)
	}
	if listener.Port != 443 {
		t.Errorf("expected port 443, got %d", listener.Port)
	}
	if listener.Protocol != gatewayv1.HTTPSProtocolType {
		t.Errorf("expected HTTPS protocol, got %s", listener.Protocol)
	}
	if listener.TLS == nil || len(listener.TLS.CertificateRefs) != 1 {
		t.Fatal("expected TLS config with 1 certificate ref")
	}
	if string(listener.TLS.CertificateRefs[0].Name) != "test-example-com-tls" {
		t.Errorf("expected secret name 'test-example-com-tls', got %q", listener.TLS.CertificateRefs[0].Name)
	}
	if listener.AllowedRoutes == nil || listener.AllowedRoutes.Namespaces == nil {
		t.Fatal("expected AllowedRoutes with namespace config")
	}
	if *listener.AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromSelector {
		t.Errorf("expected NamespacesFromSelector, got %s", *listener.AllowedRoutes.Namespaces.From)
	}
	if listener.AllowedRoutes.Namespaces.Selector == nil {
		t.Fatal("expected namespace selector")
	}
	if listener.AllowedRoutes.Namespaces.Selector.MatchLabels["kubernetes.io/metadata.name"] != "default" {
		t.Errorf("expected namespace selector for 'default', got %v", listener.AllowedRoutes.Namespaces.Selector.MatchLabels)
	}

	// Verify finalizer was added
	var route gatewayv1.HTTPRoute
	_ = r.Get(ctx, types.NamespacedName{Name: "test-route", Namespace: "default"}, &route)
	if !controllerutil.ContainsFinalizer(&route, finalizerName) {
		t.Error("expected finalizer to be present")
	}
}

func TestReconcile_IssuerAnnotation(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners:        []gatewayv1.Listener{},
		},
	}
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
			Annotations: map[string]string{
				"cert-manager.io/issuer": "letsencrypt",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"test.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(gateway, httpRoute)
	ctx := context.Background()

	_, _ = r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})
	_, _ = r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})

	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)

	if len(gw.Spec.Listeners) != 1 {
		t.Fatalf("expected 1 listener for issuer annotation, got %d", len(gw.Spec.Listeners))
	}
}

func TestReconcile_DeleteRemovesListener(t *testing.T) {
	ns := gatewayv1.Namespace("nginx-gateway")
	hostname := gatewayv1.Hostname("test.example.com")
	tlsMode := gatewayv1.TLSModeTerminate
	allowAll := gatewayv1.NamespacesFromAll

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "nginx-gateway",
			// Pre-existing owner annotation: this listener was claimed by test-route in a
			// prior reconcile. Deletion of the route should release ownership and remove it.
			Annotations: map[string]string{
				ownerAnnotationPrefix + "https-test-example-com": "default/test-route",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https-test-example-com",
					Hostname: &hostname,
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: &allowAll},
					},
					TLS: &gatewayv1.ListenerTLSConfig{
						Mode: &tlsMode,
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{Name: "test-example-com-tls", Namespace: &ns},
						},
					},
				},
			},
		},
	}

	now := metav1.NewTime(time.Now())
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-route",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer": "letsencrypt",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"test.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(gateway, httpRoute)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)

	if len(gw.Spec.Listeners) != 0 {
		t.Errorf("expected 0 listeners after deletion, got %d", len(gw.Spec.Listeners))
	}
	if _, present := gw.Annotations[ownerAnnotationPrefix+"https-test-example-com"]; present {
		t.Errorf("expected owner annotation to be released on deletion, but it persists")
	}
}

func TestReconcile_MultipleHostnames(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners:        []gatewayv1.Listener{},
		},
	}
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-route",
			Namespace: "default",
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer": "letsencrypt",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"one.example.com", "two.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(gateway, httpRoute)
	ctx := context.Background()

	// Reconcile twice: first adds finalizer, second creates listeners
	_, _ = r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "multi-route", Namespace: "default"},
	})
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "multi-route", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)

	if len(gw.Spec.Listeners) != 2 {
		t.Fatalf("expected 2 listeners, got %d", len(gw.Spec.Listeners))
	}

	names := make(map[string]bool)
	for _, l := range gw.Spec.Listeners {
		names[string(l.Name)] = true
	}
	if !names["https-one-example-com"] || !names["https-two-example-com"] {
		t.Errorf("expected listeners for both hostnames, got: %v", names)
	}
}

func TestReconcile_HostnameChangeRemovesOldListener(t *testing.T) {
	ns := gatewayv1.Namespace("nginx-gateway")
	oldHostname := gatewayv1.Hostname("old.example.com")
	tlsMode := gatewayv1.TLSModeTerminate
	allowAll := gatewayv1.NamespacesFromAll

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https-old-example-com",
					Hostname: &oldHostname,
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: &allowAll},
					},
					TLS: &gatewayv1.ListenerTLSConfig{
						Mode: &tlsMode,
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{Name: "old-example-com-tls", Namespace: &ns},
						},
					},
				},
			},
		},
	}

	// HTTPRoute has finalizer and annotation tracking the old hostname
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-route",
			Namespace:  "default",
			Finalizers: []string{finalizerName},
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer": "letsencrypt",
				managedHostnamesAnnotation:       "https-old-example-com",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"new.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(gateway, httpRoute)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)

	if len(gw.Spec.Listeners) != 1 {
		t.Fatalf("expected 1 listener, got %d", len(gw.Spec.Listeners))
	}

	if string(gw.Spec.Listeners[0].Name) != "https-new-example-com" {
		t.Errorf("expected listener 'https-new-example-com', got %q", gw.Spec.Listeners[0].Name)
	}

	// Ownership should be tracked on the Gateway, not on the route.
	expectedOwner := "default/test-route"
	if gw.Annotations[ownerAnnotationPrefix+"https-new-example-com"] != expectedOwner {
		t.Errorf("expected owner annotation for new listener, got %q",
			gw.Annotations[ownerAnnotationPrefix+"https-new-example-com"])
	}
	if _, ok := gw.Annotations[ownerAnnotationPrefix+"https-old-example-com"]; ok {
		t.Errorf("expected owner annotation for old listener to be released, but it's still present")
	}

	// Legacy route-side annotation should be stripped after migration.
	var route gatewayv1.HTTPRoute
	_ = r.Get(ctx, types.NamespacedName{Name: "test-route", Namespace: "default"}, &route)
	if v, ok := route.Annotations[managedHostnamesAnnotation]; ok {
		t.Errorf("legacy managed-hostnames annotation should have been removed, still present: %q", v)
	}
}

func TestReconcile_BootstrapSetsAnnotation(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners:        []gatewayv1.Listener{},
		},
	}

	// Existing route with finalizer but no managed-hostnames annotation (pre-upgrade)
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-route",
			Namespace:  "default",
			Finalizers: []string{finalizerName},
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer": "letsencrypt",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(gateway, httpRoute)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Owner annotation should be set on the Gateway after first reconcile.
	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)
	if got := gw.Annotations[ownerAnnotationPrefix+"https-example-com"]; got != "default/test-route" {
		t.Errorf("expected owner annotation 'default/test-route', got %q", got)
	}
}

func TestReconcile_ManualListenerNotRemoved(t *testing.T) {
	manualHostname := gatewayv1.Hostname("manual.example.com")
	tlsMode := gatewayv1.TLSModeTerminate
	allowAll := gatewayv1.NamespacesFromAll
	ns := gatewayv1.Namespace("nginx-gateway")

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https-manual-example-com",
					Hostname: &manualHostname,
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: &allowAll},
					},
					TLS: &gatewayv1.ListenerTLSConfig{
						Mode: &tlsMode,
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{Name: "manual-example-com-tls", Namespace: &ns},
						},
					},
				},
			},
		},
	}

	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-route",
			Namespace:  "default",
			Finalizers: []string{finalizerName},
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer": "letsencrypt",
				managedHostnamesAnnotation:       "https-other-example-com",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"app.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(gateway, httpRoute)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)

	// Manual listener should still be there, plus the new one
	if len(gw.Spec.Listeners) != 2 {
		t.Fatalf("expected 2 listeners (manual + new), got %d", len(gw.Spec.Listeners))
	}

	names := make(map[string]bool)
	for _, l := range gw.Spec.Listeners {
		names[string(l.Name)] = true
	}
	if !names["https-manual-example-com"] {
		t.Error("manual listener was incorrectly removed")
	}
	if !names["https-app-example-com"] {
		t.Error("new listener was not added")
	}
}

func TestReconcile_DisallowedHostname_RecordsEvent(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-bad"}}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners:        []gatewayv1.Listener{},
		},
	}
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-route",
			Namespace: "tenant-bad",
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer": "letsencrypt",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"evil.hacker.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(ns, gateway, httpRoute)
	fakeRecorder := newFakeEventRecorder(10)
	r.Recorder = fakeRecorder
	ctx := context.Background()

	// First reconcile: add finalizer
	_, _ = r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "bad-route", Namespace: "tenant-bad"},
	})
	// Second reconcile: attempt to create listener (should fail validation)
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "bad-route", Namespace: "tenant-bad"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have no listeners
	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)
	if len(gw.Spec.Listeners) != 0 {
		t.Errorf("expected 0 listeners for disallowed hostname, got %d", len(gw.Spec.Listeners))
	}

	// Check event was recorded
	select {
	case event := <-fakeRecorder.Events:
		if event == "" {
			t.Error("expected a non-empty event")
		}
	default:
		t.Error("expected an event to be recorded for hostname validation failure")
	}
}

func TestReconcile_NotFound(t *testing.T) {
	r := newReconciler()
	ctx := context.Background()

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error for not-found: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("should not requeue for not-found")
	}
}

func TestNamespaceToHTTPRoutes_EnqueuesManagedRoutes(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tenant-abc",
			Annotations: map[string]string{
				"gateway-auto-listener/allowed-hostnames": "custom.org",
			},
		},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners:        []gatewayv1.Listener{},
		},
	}
	managedRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "managed-route",
			Namespace:  "tenant-abc",
			Finalizers: []string{finalizerName},
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer": "letsencrypt",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"app.custom.org"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}
	unmanagedRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unmanaged-route",
			Namespace: "tenant-abc",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"other.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}
	otherNSRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "other-ns-route",
			Namespace:  "tenant-xyz",
			Finalizers: []string{finalizerName},
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer": "letsencrypt",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"app.other.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(ns, gateway, managedRoute, unmanagedRoute, otherNSRoute)
	ctx := context.Background()

	requests := r.namespaceToHTTPRoutes(ctx, ns)

	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "managed-route" || requests[0].Namespace != "tenant-abc" {
		t.Errorf("expected managed-route in tenant-abc, got %s/%s", requests[0].Namespace, requests[0].Name)
	}
}

func TestNamespaceToHTTPRoutes_SkipsNonMatchingPrefix(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system",
			Annotations: map[string]string{
				"gateway-auto-listener/allowed-hostnames": "something.com",
			},
		},
	}

	r := newReconciler(ns)
	ctx := context.Background()

	requests := r.namespaceToHTTPRoutes(ctx, ns)

	if len(requests) != 0 {
		t.Errorf("expected 0 requests for non-matching prefix, got %d", len(requests))
	}
}

func TestNamespaceAnnotationChanged_Predicate(t *testing.T) {
	r := newReconciler()
	pred := r.namespaceAnnotationChanged()

	t.Run("create with annotation", func(t *testing.T) {
		e := event.CreateEvent{
			Object: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "tenant-new",
					Annotations: map[string]string{
						"gateway-auto-listener/allowed-hostnames": "example.com",
					},
				},
			},
		}
		if !pred.Create(e) {
			t.Error("expected create with annotation to return true")
		}
	})

	t.Run("create without annotation", func(t *testing.T) {
		e := event.CreateEvent{
			Object: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "tenant-plain"},
			},
		}
		if pred.Create(e) {
			t.Error("expected create without annotation to return false")
		}
	})

	t.Run("update annotation changed", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "tenant-x",
					Annotations: map[string]string{
						"gateway-auto-listener/allowed-hostnames": "old.com",
					},
				},
			},
			ObjectNew: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "tenant-x",
					Annotations: map[string]string{
						"gateway-auto-listener/allowed-hostnames": "new.com",
					},
				},
			},
		}
		if !pred.Update(e) {
			t.Error("expected update with changed annotation to return true")
		}
	})

	t.Run("update annotation added", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "tenant-x"},
			},
			ObjectNew: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "tenant-x",
					Annotations: map[string]string{
						"gateway-auto-listener/allowed-hostnames": "new.com",
					},
				},
			},
		}
		if !pred.Update(e) {
			t.Error("expected update with added annotation to return true")
		}
	})

	t.Run("update unrelated change", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "tenant-x",
					Labels: map[string]string{"foo": "bar"},
				},
			},
			ObjectNew: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "tenant-x",
					Labels: map[string]string{"foo": "baz"},
				},
			},
		}
		if pred.Update(e) {
			t.Error("expected update without annotation change to return false")
		}
	})

	t.Run("delete returns false", func(t *testing.T) {
		e := event.DeleteEvent{
			Object: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "tenant-gone"},
			},
		}
		if pred.Delete(e) {
			t.Error("expected delete to return false")
		}
	})
}

// TestReconcile_TenantAnnotationCannotDeleteForeignListener verifies the security fix
// for the cross-tenant listener-deletion bug. A tenant-controlled HTTPRoute that puts
// a foreign listener name in the legacy managed-hostnames annotation must not cause
// that listener to be removed.
func TestReconcile_TenantAnnotationCannotDeleteForeignListener(t *testing.T) {
	victimHostname := gatewayv1.Hostname("victim.example.com")
	tlsMode := gatewayv1.TLSModeTerminate
	allowAll := gatewayv1.NamespacesFromAll
	ns := gatewayv1.Namespace("nginx-gateway")

	// Pre-existing listener owned by victim-route.
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "nginx-gateway",
			Annotations: map[string]string{
				ownerAnnotationPrefix + "https-victim-example-com": "default/victim-route",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https-victim-example-com",
					Hostname: &victimHostname,
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: &allowAll},
					},
					TLS: &gatewayv1.ListenerTLSConfig{
						Mode: &tlsMode,
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{Name: "victim-example-com-tls", Namespace: &ns},
						},
					},
				},
			},
		},
	}

	// Attacker's tenant route claims it previously managed the victim's listener
	// (legacy annotation forgery) and asks to remove it by not listing the hostname.
	tenantNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-attacker"}}
	attackerRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "attacker-route",
			Namespace:  "tenant-attacker",
			Finalizers: []string{finalizerName},
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer":          "letsencrypt",
				"gateway-auto-listener/managed-hostnames": "https-victim-example-com",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames:       []gatewayv1.Hostname{"app.tenant-attacker.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(gateway, attackerRoute, tenantNS)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "attacker-route", Namespace: "tenant-attacker"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)

	// Victim listener must still be present.
	found := false
	for _, l := range gw.Spec.Listeners {
		if string(l.Name) == "https-victim-example-com" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("victim listener was deleted via tenant-controllable annotation — security regression")
	}

	// Owner of victim listener must remain victim-route.
	if got := gw.Annotations[ownerAnnotationPrefix+"https-victim-example-com"]; got != "default/victim-route" {
		t.Errorf("victim listener ownership changed to %q (expected default/victim-route)", got)
	}
}

// TestReconcile_SkipsRouteWithoutManagedGatewayParentRef verifies the controller
// ignores HTTPRoutes that don't list the managed Gateway as a ParentRef.
func TestReconcile_SkipsRouteWithoutManagedGatewayParentRef(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners:        []gatewayv1.Listener{},
		},
	}
	otherGwNS := gatewayv1.Namespace("other-namespace")
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-route",
			Namespace: "default",
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer": "letsencrypt",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"unrelated.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName("other-gateway"), Namespace: &otherGwNS},
				},
			},
		},
	}

	r := newReconciler(gateway, httpRoute)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "other-route", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)
	if len(gw.Spec.Listeners) != 0 {
		t.Errorf("expected 0 listeners (route doesn't reference managed Gateway), got %d", len(gw.Spec.Listeners))
	}
}

// TestRemoveListeners_TenantDeletionCannotRemoveForeignListener guards BLOCKER #1
// from the implementation review: the finalizer (delete) path must not derive
// listener names from tenant-writable fields. An attacker route with `Hostnames:
// [victim]` or with a forged legacy `managed-hostnames` annotation must not cause
// the victim's listener to be removed when the attacker's route is deleted.
func TestRemoveListeners_TenantDeletionCannotRemoveForeignListener(t *testing.T) {
	victimHostname := gatewayv1.Hostname("victim.example.com")
	tlsMode := gatewayv1.TLSModeTerminate
	allowAll := gatewayv1.NamespacesFromAll
	ns := gatewayv1.Namespace("nginx-gateway")

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "nginx-gateway",
			Annotations: map[string]string{
				ownerAnnotationPrefix + "https-victim-example-com": "default/victim-route",
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https-victim-example-com",
					Hostname: &victimHostname,
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: &allowAll},
					},
					TLS: &gatewayv1.ListenerTLSConfig{
						Mode: &tlsMode,
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{Name: "victim-example-com-tls", Namespace: &ns},
						},
					},
				},
			},
		},
	}

	now := metav1.NewTime(time.Now())
	tenantNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-attacker"}}
	attackerRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "attacker-route",
			Namespace:         "tenant-attacker",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer":          "letsencrypt",
				"gateway-auto-listener/managed-hostnames": "https-victim-example-com",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			// Spec.Hostnames also targets victim — pre-fix code would derive listeners
			// to remove from this and delete the foreign listener.
			Hostnames:       []gatewayv1.Hostname{"victim.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(gateway, attackerRoute, tenantNS)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "attacker-route", Namespace: "tenant-attacker"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)

	found := false
	for _, l := range gw.Spec.Listeners {
		if string(l.Name) == "https-victim-example-com" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("victim listener was deleted via attacker-route deletion — security regression in removeListeners")
	}
	if got := gw.Annotations[ownerAnnotationPrefix+"https-victim-example-com"]; got != "default/victim-route" {
		t.Errorf("victim ownership annotation changed to %q (expected default/victim-route)", got)
	}
}

// TestReconcile_LegacyAnnotationCannotForgeOnUnownedListener guards BLOCKER #2.
// During the v0.1.x → v0.2.0 migration window, legitimate listeners have no owner
// annotations yet. A tenant attacker reconciling first with a forged legacy
// managed-hostnames annotation must not be able to claim a victim hostname's
// listener even though no owner annotation exists, because the listener's actual
// hostname fails validateHostname() for the attacker's namespace.
func TestReconcile_LegacyAnnotationCannotForgeOnUnownedListener(t *testing.T) {
	victimHostname := gatewayv1.Hostname("victim.example.com")
	tlsMode := gatewayv1.TLSModeTerminate
	allowAll := gatewayv1.NamespacesFromAll
	ns := gatewayv1.Namespace("nginx-gateway")

	// Gateway has the victim listener but NO owner annotation — simulating the
	// migration window before v0.2.0 has annotated existing listeners.
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "nginx",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https-victim-example-com",
					Hostname: &victimHostname,
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: &allowAll},
					},
					TLS: &gatewayv1.ListenerTLSConfig{
						Mode: &tlsMode,
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{Name: "victim-example-com-tls", Namespace: &ns},
						},
					},
				},
			},
		},
	}

	tenantNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-attacker"}}
	attackerRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "attacker-route",
			Namespace:  "tenant-attacker",
			Finalizers: []string{finalizerName},
			Annotations: map[string]string{
				"cert-manager.io/cluster-issuer":          "letsencrypt",
				"gateway-auto-listener/managed-hostnames": "https-victim-example-com",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			// Attacker's own (validation-passing) hostname; legacy-claim is the attack.
			Hostnames:       []gatewayv1.Hostname{"app.tenant-attacker.example.com"},
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToDefault()},
		},
	}

	r := newReconciler(gateway, attackerRoute, tenantNS)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "attacker-route", Namespace: "tenant-attacker"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gw gatewayv1.Gateway
	_ = r.Get(ctx, types.NamespacedName{Name: "default", Namespace: "nginx-gateway"}, &gw)

	found := false
	for _, l := range gw.Spec.Listeners {
		if string(l.Name) == "https-victim-example-com" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("victim listener was claimed/deleted via legacy-annotation forgery on unowned listener")
	}
	if got := gw.Annotations[ownerAnnotationPrefix+"https-victim-example-com"]; got != "" {
		t.Errorf("attacker successfully claimed ownership of victim listener: %q", got)
	}
}
