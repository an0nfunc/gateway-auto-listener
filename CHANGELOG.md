# Changelog

## v0.2.3

- The allowed-hostnames namespace annotation now supports `*.apex`
  wildcard entries: such an entry allows any hostname strictly below
  `apex` (the apex itself is not matched). Malformed or empty entries
  (`*`, `*.`, `*foo.bar`, `*..`) match nothing and fail closed.
  Previously a wildcard entry was compared literally, so it only
  matched the wildcard hostname `*.apex` itself — concrete hostnames
  below the apex were rejected.

## v0.2.2

- Migrated from the deprecated `record.EventRecorder` to the new
  `k8s.io/client-go/tools/events.EventRecorder` (controller-runtime
  v0.23 ships both; the old API was deprecation-warned by staticcheck).
  Code change is a no-op for chart consumers, but the chart ClusterRole
  now also grants `events.k8s.io/events: create,patch` alongside the
  legacy core `events` grant. `helm upgrade` adds the new permission
  automatically.

## v0.2.1

- Bump Go toolchain to 1.25.9 to pick up stdlib fixes for GO-2026-4870
  (TLS 1.3 KeyUpdate DoS), GO-2026-4946 (x509 inefficient policy
  validation), and GO-2026-4947 (x509 chain-building work). v0.2.0 was
  built with go 1.25.8 and tripped govulncheck in CI.

## v0.2.0

### Security

- **Cross-tenant listener tampering fixed.** Ownership of managed listeners is now tracked
  via annotations on the Gateway resource (`gateway-auto-listener.itsh.dev/owner.<listener>`)
  instead of the route-local `gateway-auto-listener/managed-hostnames` annotation. Tenants
  can no longer forge that annotation to delete or hijack another tenant's listeners.
- **HTTPRoute ParentRefs are now checked.** A route must list the managed Gateway in its
  `spec.parentRefs` to be processed. Routes that target unrelated Gateways but happen to
  carry a cert-manager annotation no longer create phantom listeners on the managed Gateway.
- **`hostnameValidation.enabled` defaults to `true`** in the chart. Multi-tenant deployments
  get safe defaults out of the box. Set `hostnameValidation.enabled: false` explicitly if
  you trust every namespace to claim any hostname.

### Migration

The first reconcile after upgrade reads each route's existing `managed-hostnames`
annotation exactly once to seed the new Gateway-side ownership annotations. Two
safety gates make this fallback safe even during the migration window when
listeners have no owner annotations yet:

1. The fallback never claims a listener whose Gateway-side owner annotation already
   names a different route.
2. The fallback never claims a listener whose actual hostname does not pass
   `validateHostname()` for the route's namespace. This blocks the original
   cross-tenant attack: an attacker tenant can no longer claim `victim.com`
   because that hostname fails validation against `tenant-attacker`.

After the first reconcile:

- The legacy annotation is removed from the route.
- All ownership decisions consult the Gateway annotations only.

No manual action is required for existing deployments. The deletion path
(finalizer) also derives the listener set strictly from Gateway-side ownership,
so a tenant deleting a forged route can no longer cause foreign-listener removal.

For cluster operators who relied on the previous `from: All` behavior of allowing routes
from any namespace, ensure routes you want auto-listened set `parentRefs` correctly.

## v0.1.4 and earlier

See git history.
