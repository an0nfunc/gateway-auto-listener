package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

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

func newReconciler(objs ...client.Object) *HTTPRouteReconciler {
	cb := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...)
	cb = cb.WithStatusSubresource(objs...)

	return &HTTPRouteReconciler{
		Client:                     cb.Build(),
		Scheme:                     scheme.Scheme,
		Recorder:                   record.NewFakeRecorder(10),
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
			Hostnames: []gatewayv1.Hostname{"test.example.com"},
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
			Hostnames: []gatewayv1.Hostname{"test.example.com"},
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
			Hostnames: []gatewayv1.Hostname{"test.example.com"},
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
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "nginx-gateway"},
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
			Hostnames: []gatewayv1.Hostname{"test.example.com"},
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
			Hostnames: []gatewayv1.Hostname{"one.example.com", "two.example.com"},
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
			Hostnames: []gatewayv1.Hostname{"new.example.com"},
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

	// Verify annotation was updated
	var route gatewayv1.HTTPRoute
	_ = r.Get(ctx, types.NamespacedName{Name: "test-route", Namespace: "default"}, &route)
	if route.Annotations[managedHostnamesAnnotation] != "https-new-example-com" {
		t.Errorf("expected annotation 'https-new-example-com', got %q", route.Annotations[managedHostnamesAnnotation])
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
			Hostnames: []gatewayv1.Hostname{"example.com"},
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

	// Annotation should be set after first reconcile
	var route gatewayv1.HTTPRoute
	_ = r.Get(ctx, types.NamespacedName{Name: "test-route", Namespace: "default"}, &route)
	if route.Annotations[managedHostnamesAnnotation] != "https-example-com" {
		t.Errorf("expected annotation 'https-example-com', got %q", route.Annotations[managedHostnamesAnnotation])
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
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
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
			Hostnames: []gatewayv1.Hostname{"evil.hacker.com"},
		},
	}

	r := newReconciler(ns, gateway, httpRoute)
	fakeRecorder := record.NewFakeRecorder(10)
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
