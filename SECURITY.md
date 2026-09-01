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

## Refreshing a pinned base image after a CVE

The `container-scan` CI job runs Trivy with `severity: CRITICAL,HIGH`,
`ignore-unfixed: true`, and `exit-code: 1` against the image built from
[`Dockerfile`](Dockerfile), whose runtime stage is pinned by digest. Those two
facts together mean a CVE published against a package in the pinned base image
fails **every branch, including docs-only commits**, with nothing in the repo
having changed. This is the accepted cost of digest pinning — an immutable base
is worth chasing a digest occasionally — and it has now happened more than
once (curl in #56, then sqlite). Do not "fix" it by switching to a floating tag.

Dependabot (`.github/dependabot.yml`, docker ecosystem, daily) opens the digest
bump on its own, so the fix is usually already sitting in an open PR before
anyone investigates. Check there first.

To refresh by hand, or to confirm a Dependabot bump actually clears the CVE
before merging it:

```bash
# 1. What does the tag point at now?
curl -sI -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
  https://registry.access.redhat.com/v2/ubi9/ubi-minimal/manifests/latest \
  | grep -i docker-content-digest

# 2. Does that digest actually carry the fixed package? A newer digest is not
#    automatically a patched one — check the version, not the date.
docker run --rm --platform linux/amd64 \
  registry.access.redhat.com/ubi9/ubi-minimal@sha256:<new> rpm -q sqlite-libs

# 3. Scan the candidate base with the same settings CI uses (exit 0 = clear).
docker run --rm ghcr.io/aquasecurity/trivy:latest image \
  --platform linux/amd64 --severity CRITICAL,HIGH --ignore-unfixed --exit-code 1 \
  registry.access.redhat.com/ubi9/ubi-minimal@sha256:<new>
```

Then update the digest in `Dockerfile` and land it as its own commit, not as a
drive-by inside an unrelated change — it blocks everyone, and it should be
revertable on its own.

Two things worth knowing before you widen the fix:

- **The builder stage is not scanned.** `Dockerfile`'s `ubi9/go-toolset` stage
  contributes only the statically linked `manager` binary
  (`CGO_ENABLED=0`); none of its RPMs reach the final image, so a CVE in the
  builder base never fails `container-scan` and bumping it will not fix one.
  Verify with `rpm -qa` in the built image rather than assuming.
- **On an arm64 workstation, build natively.** `docker build --platform
  linux/amd64` runs the builder's Go under emulation, which miscompiles it
  (`fatal error: found pointer to free object`) — a local-only failure, since
  the CI runner is amd64-native. The Dockerfile already cross-compiles
  (`GOOS=linux GOARCH=amd64`), so pin only the *runtime* stage to amd64 and let
  the builder run native if you need to reproduce the CI image locally.

## Supported versions

This project is `alpha` maturity (see the CSV's `maturity` field) with a
single `alpha` release channel. Security fixes land on `main` and are
included in the next tagged release — there is currently no backport policy
across multiple release lines.

## Disclosure

We'll credit reporters (unless anonymity is requested) in the release notes
of the fix once a patched version has shipped.
