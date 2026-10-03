# Security Policy

## Reporting a Vulnerability

Please report suspected security vulnerabilities **privately** — do not open a
public GitHub issue for anything that could be actively exploited.

- Open a [private security advisory](https://github.com/PulseSRE/pulse-operator/security/advisories/new)
  on this repository (preferred), or
- Email the maintainer listed in [MAINTAINERS](bundle/manifests/pulse-operator.clusterserviceversion.yaml)
  (`amobrem@redhat.com`) with a description, reproduction steps, and impact.

We aim to acknowledge reports within 5 business days.

## Scope and known risk areas

This operator manages RBAC on behalf of every `OpenShiftPulse` instance it
reconciles, and its own ServiceAccount is granted broad permissions to do so
(see [`config/rbac/role.yaml`](config/rbac/role.yaml) — the RBAC scoping
rationale is documented inline there). Reports concerning any of the
following are especially welcome:

- Privilege escalation via the operator's ClusterRole (`create`/`update` on
  `clusterroles`/`clusterrolebindings`)
- Any path by which a managed `OpenShiftPulse` CR's spec (`spec.agent`,
  `spec.ui`, etc.) can be used to influence resources outside its own
  namespace, or to escalate the generated agent/UI ClusterRoles beyond what
  `spec.agent.allowWriteOperations` / `spec.agent.allowSecretAccess` grant
  today
- OAuthClient / Route handling that could allow a redirect URI or grant
  method to be hijacked (see the `grantMethod: auto` rationale in
  `internal/controller/ui_reconciler.go`)
- Secrets (ws-token, oauth cookie/client secret, PostgreSQL password) leaking
  via logs, events, or status fields

## Refreshing container dependencies after a CVE

The `container-scan` job builds the operator image and runs Trivy with
`severity: CRITICAL,HIGH`, `ignore-unfixed: true`, and `exit-code: 1`.
The runtime base is digest-pinned, and `microdnf update` applies available
package errata during each build. A new fixable vulnerability can therefore
change CI results even when the source tree has not changed.

Dependabot checks Docker digests daily. Review its existing PRs before opening
another update. To validate a candidate, build the complete operator image and
scan that image with the same Trivy settings; scanning only the base does not
exercise the package update or inspect the operator binary. Preserve the
`microdnf update` step when resolving an older digest PR against current main.

The builder contributes only the statically linked manager executable
(`CGO_ENABLED=0`) to the runtime stage. Updating builder RPMs alone does not
repair a runtime RPM vulnerability. Updating Go modules or the Go toolchain
may still be needed for vulnerabilities in the executable.

Kubernetes Go modules and controller-runtime are grouped in Dependabot because
independent minor updates can produce incompatible generated validation code.
Resolve them together, run `go mod tidy`, and run the full envtest suite before
landing the update.

## Supported versions

This project is `alpha` maturity (see the CSV's `maturity` field) with a
single `alpha` release channel. Security fixes land on `main` and are
included in the next tagged release — there is currently no backport policy
across multiple release lines.

## Disclosure

We'll credit reporters (unless anonymity is requested) in the release notes
of the fix once a patched version has shipped.
