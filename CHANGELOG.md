# Changelog

## v0.2.5

- The project moved from `an0nfunc` to the `itsh-cloud` organisation. A GitHub
  transfer redirects the repository and the git remote, but it does **not** move
  GHCR packages, so `ghcr.io/an0nfunc/gateway-auto-listener` is frozen at v0.2.4
  and will never receive another version. Update your references:
  - Image: `ghcr.io/itsh-cloud/gateway-auto-listener`
  - Chart: `oci://ghcr.io/itsh-cloud/charts/gateway-auto-listener`
  - Module: `go install github.com/itsh-cloud/gateway-auto-listener/cmd/gateway-auto-listener@latest`
- **Breaking:** the leader election lease is renamed from
  `gateway-auto-listener.an0nfunc.github.io` to `gateway-auto-listener.itsh.dev`.
  Pods on either side of the rename do not see each other's lease, so a rolling
  update leaves two active leaders reconciling the same Gateway.

  **Migration:** scale the deployment to 0, wait for the old lease to expire,
  then roll out v0.2.5. If you run under a GitOps controller with self-heal,
  set the replica count to 0 through the controller rather than with
  `kubectl scale`, which will simply be reverted.
- The chart is now actually published by CI. Earlier versions documented a Helm
  OCI install that no release ever pushed, so `helm install` from the registry
  could only fail. Releases also fail fast if `Chart.yaml` and the tag disagree.
- Dependency updates: gateway-api v1.6.1, Go toolchain 1.26.6 and
  golang.org/x/text v0.39.0 to clear govulncheck findings (GO-2026-6218,
  GO-2026-6090, GO-2026-6089, GO-2026-5972, GO-2026-5970, GO-2026-5026).

## v0.2.4

- Dependency updates: controller-runtime v0.24.1, k8s.io v0.36.2,
  gateway-api v1.6.0, actions/checkout v7. Go toolchain pinned to
  1.26.5 and golang.org/x/net bumped to v0.55.0 to clear govulncheck
  findings (GO-2026-5856, GO-2026-5039, GO-2026-5038, GO-2026-5037,
  GO-2026-5026).
- Note: the v0.2.3 tag exists but produced no release artifacts (CI
  failed on the govulncheck findings above before building). v0.2.4 is
  the first release shipping the wildcard allowed-hostnames support
  listed under v0.2.3.

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
