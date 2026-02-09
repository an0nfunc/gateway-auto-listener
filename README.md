# gateway-auto-listener

[![CI](https://github.com/an0nfunc/gateway-auto-listener/actions/workflows/ci.yaml/badge.svg)](https://github.com/an0nfunc/gateway-auto-listener/actions/workflows/ci.yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/an0nfunc/gateway-auto-listener)](https://goreportcard.com/report/github.com/an0nfunc/gateway-auto-listener)
[![License](https://img.shields.io/github/license/an0nfunc/gateway-auto-listener)](LICENSE)
[![Release](https://img.shields.io/github/v/release/an0nfunc/gateway-auto-listener)](https://github.com/an0nfunc/gateway-auto-listener/releases)

Kubernetes controller that automatically creates Gateway API HTTPS listeners from HTTPRoutes annotated with cert-manager issuer annotations.

## Problem

The Gateway API requires HTTPS listeners to be explicitly defined on a Gateway before HTTPRoutes can attach to them. When using cert-manager to provision TLS certificates, you need to manually create a listener for each hostname. This controller closes the gap by watching HTTPRoutes and auto-creating the corresponding Gateway listeners with TLS configuration.

## How It Works

```
HTTPRoute (with cert-manager annotation)
    |
    v
gateway-auto-listener detects new/updated HTTPRoute
    |
    v
Creates HTTPS listener on Gateway
  - Port 443, TLS terminate mode
  - Certificate reference: <hostname>-tls
  - AllowedRoutes: from all namespaces
    |
    v
cert-manager sees the listener and provisions a certificate
    |
    v
HTTPRoute attaches to the new listener
```

### Comparison with cert-manager gateway-shim

cert-manager's [gateway-shim](https://cert-manager.io/docs/usage/gateway/) works in the opposite direction: given an existing Gateway listener, it creates a Certificate resource. **gateway-auto-listener** creates the listener itself from HTTPRoute annotations — they complement each other.

## Prerequisites

- Kubernetes >= 1.28
- [Gateway API](https://gateway-api.sigs.k8s.io/) CRDs installed
- [cert-manager](https://cert-manager.io/) installed
- A Gateway implementation (e.g., NGINX Gateway Fabric, Envoy Gateway, Istio)

## Compatibility

| Implementation | Status |
|----------------|--------|
| NGINX Gateway Fabric | Tested |
| Envoy Gateway | Untested (should work) |
| Istio | Untested (should work) |
| Other | Untested |

## Quick Start

```bash
# Install CRDs (if not already present)
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml

# Deploy gateway-auto-listener
kubectl apply -f https://raw.githubusercontent.com/an0nfunc/gateway-auto-listener/main/deploy/manifests.yaml

# Create an HTTPRoute with a cert-manager annotation
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-app
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
spec:
  hostnames:
    - my-app.example.com
  parentRefs:
    - name: default
      namespace: nginx-gateway
  rules:
    - backendRefs:
        - name: my-app
          port: 80
EOF
```

The controller will automatically create an HTTPS listener for `my-app.example.com` on the Gateway.

## Installation

### Helm

```bash
helm install gateway-auto-listener oci://ghcr.io/an0nfunc/gateway-auto-listener/chart \
  --namespace nginx-gateway \
  --set gateway.name=default \
  --set gateway.namespace=nginx-gateway
```

Or from source:

```bash
git clone https://github.com/an0nfunc/gateway-auto-listener.git
helm install gateway-auto-listener ./chart/gateway-auto-listener \
  --namespace nginx-gateway
```

### Raw Manifests

```bash
kubectl apply -f https://raw.githubusercontent.com/an0nfunc/gateway-auto-listener/main/deploy/manifests.yaml
```

## Configuration

| Flag | Default | Description |
|------|---------|-------------|
| `--gateway-name` | `default` | Name of the Gateway to manage listeners on |
| `--gateway-namespace` | `nginx-gateway` | Namespace of the Gateway |
| `--validated-ns-prefix` | `""` (disabled) | Namespace prefix triggering hostname validation |
| `--allowed-domain-suffix` | `""` | Domain suffix for tenant default subdomains |
| `--allowed-hostnames-annotation` | `gateway-auto-listener/allowed-hostnames` | Namespace annotation key for allowed custom hostnames |
| `--metrics-bind-address` | `:8080` | Metrics endpoint bind address |
| `--health-probe-bind-address` | `:8081` | Health probe bind address |
| `--version` | | Print version and exit |

### Helm Values

See [values.yaml](chart/gateway-auto-listener/values.yaml) for all available Helm values.

## Hostname Validation

When `--validated-ns-prefix` is set (e.g., `tenant-`), namespaces matching that prefix are subject to hostname validation:

1. **Default subdomain**: `<anything>.<namespace>.<domain-suffix>` is always allowed (when `--allowed-domain-suffix` is set).
2. **Custom domains**: Listed in the namespace annotation (comma-separated). Subdomains are also allowed.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: tenant-acme
  annotations:
    gateway-auto-listener/allowed-hostnames: "acme.com, shop.acme.org"
```

Namespaces not matching the prefix can use any hostname.

## Uninstall

Before uninstalling, ensure you clean up managed listeners. The controller uses finalizers to remove listeners when HTTPRoutes are deleted. If you remove the controller first, finalizers on existing HTTPRoutes will prevent their deletion.

```bash
# Option 1: Delete all managed HTTPRoutes first, then uninstall
kubectl delete httproutes -A -l cert-manager.io/cluster-issuer

# Option 2: Remove finalizers manually
kubectl get httproutes -A -o json | jq '.items[] | select(.metadata.finalizers[]? == "gateway-auto-listener/finalizer") | .metadata.name + " -n " + .metadata.namespace' -r | xargs -I {} kubectl patch httproute {} --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]'

# Then uninstall
helm uninstall gateway-auto-listener
# or
kubectl delete -f deploy/manifests.yaml
```

## Troubleshooting

**Listener not created**: Check that the HTTPRoute has a `cert-manager.io/cluster-issuer` or `cert-manager.io/issuer` annotation.

**Hostname rejected**: Check the namespace annotation for allowed hostnames and verify the `--validated-ns-prefix` and `--allowed-domain-suffix` flags.

**Finalizer stuck on HTTPRoute**: The controller must be running to process finalizer removal. If the controller is gone, manually remove the finalizer.

## License

Apache 2.0 - see [LICENSE](LICENSE).
