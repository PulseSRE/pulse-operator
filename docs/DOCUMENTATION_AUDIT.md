# Documentation audit

Reviewed against operator main `11069f857fc56c3edbdd0c98308d0fa7e4dcc09c` on 2026-10-11. This inventory covers all tracked Markdown and Pages source files in that revision. It records documentation review, not cluster acceptance, release publication or deployment.

| File | Disposition |
| --- | --- |
| README.md | Corrected stale manual catalog publication instructions that contradicted the release workflow and could drop upgrade history. Replaced an old release tag example, removed a fixed installation-time promise, corrected PVC gating claims, qualified NetworkPolicy scope and cached catalog discovery, and clarified live controller scope. Fixed the envtest helper path. Install/CR/status/troubleshooting sections checked against reconcilers, CRD, Makefile and release workflow. |
| SECURITY.md | Retained disclosure and support policy; documented the current minimum compiler and x/net security baseline and the distinction between RPM and Go binary remediation. |
| CHANGELOG.md | Added merged changes under Unreleased. Historical tagged entries remain historical; no new release or deployment is implied. |
| .claude/skills/verify/SKILL.md | Retained build/envtest checks; replaced unscoped live mutation/start instructions with the existing acceptance-runner workflow and canonical example links. |
| docs/CLUSTER_ACCEPTANCE.md | Retained after comparison with runner argument/prerequisite/report behavior and its 16 passing mocked tests. Cluster execution and incident acceptance remain unproven. |
| docs/index.html | Retained capability/evidence distinctions and added a link to security/build prerequisites. No Pages publication performed by this audit. |
| docs/_config.yml | Retained `theme: null`; Pages source is the standalone HTML document. |

Validation: local Markdown file targets and Pages HTML parse checked; `git diff --check` passed; all 16 existing acceptance-runner unit tests passed without cluster calls. Live URLs and visual browser rendering were not revalidated. Example deployment/release commands were inspected, not executed.

The follow-up offline kit adds `examples/cluster-test-kit/RUNBOOK.md` and three
Python helpers/tests, linked from the cluster acceptance guide. Its seven unit
tests cover generated fixture scope/security, deterministic faults, invalid
inputs, plan-only behavior, explicit-context reads, log failure retention and
timeout handling. No cluster operations or Pulse runtime were started.
