# Policy on Feature Gate Graduation 

## 1. Overview

The NVIDIA DRA Driver for GPUs uses Kubernetes-style feature gates to manage
the lifecycle of new capabilities. This document defines the graduation
criteria — what it takes for a feature gate to move from Alpha → Beta →
Stable, and when operators can rely on a feature in production.

The criteria are inspired by upstream Kubernetes graduation patterns and specifically various DRA KEPs but
intentionally more lightweight. The upstream KEP process requires PRR questionnaires, SIG sign-offs, and
conformance tests — necessary for a project with hundreds of contributors, but not for a driver that is still taking shape.
This policy captures the essentials — testing, documentation, observability, and real-world feedback — and will evolve as the project matures.

## 2. Feature Gate Stages

> **Note:** Some existing gates (`IMEXDaemonsWithDNSNames`, `ComputeDomainCliques`,
> `CrashOnNVLinkFabricErrors`) were introduced directly at Beta to fix some critical bugs.
> The criteria below apply to **new feature gates going forward** and to **promotions of existing gates** to the next stage.


### 2.1 Alpha

**Default:** `false` (opt-in)
**Signal:** "Try it out and give us feedback."

- Hidden behind a feature gate that defaults to `false`
- Not guaranteed to be API-stable
- May be removed or changed without notice
- **Not recommended for production use**

#### Alpha Entry Requirements

| # | Requirement | Evidence |
|---|-------------|----------|
| A1 | Feature gate registered in `pkg/featuregates/featuregates.go` with `PreRelease: featuregate.Alpha` and `Default: false` | Link to code |
| A2 | Basic unit tests for core logic | Test file links |
| A3 | Feature gate validation tests (mutual exclusivity, dependencies) | Test file links |
| A4 | No panics or data races when the gate is enabled | Test Logs |
| A5 | Basic inline code documentation (GoDoc) | Code review |

### 2.2 Beta

**Default:** `true` (opt-out)
**Signal:** "We're confident in the design. Early production use is
encouraged."

- Enabled by default
- API-stable; breaking changes require a migration path
- May still be disabled via the feature gate if issues arise

**Minimum soak:** at least **one release cycle at Alpha** before promoting.

#### Beta Graduation Requirements

All Alpha requirements, plus:

| # | Requirement | Evidence |
|---|-------------|----------|
| B1 | All critical bugs from Alpha are fixed | Linked issues closed |
| B2 | BATS tests covering primary user workflows passing in CI | Test + CI links |
| B3 | Negative / error-path tests | Test file links |
| B4 | Prometheus metrics for key operational signals (where applicable) | Metric names in proposal |
| B5 | User-facing documentation and Helm chart values with defaults | Doc + Helm diff |
| B6 | Enable → disable cycle does not corrupt state | Test or manual report |
| B7 | Version skew tested (newer driver + older Kubelet, and vice versa) | Test or manual report |
| B8 | At least one release at Alpha with no P0/P1 regressions | Release notes |
| B9 | Upstream K8s dependency at Beta, or fallback documented | KEP status link |
| B10 | No measurable performance regression | Benchmark or profiling |

### 2.3 Stable (GA) — Production Grade

**Default:** `true`, **Locked:** feature gate cannot be disabled
**Signal:** "This is production-grade. We stand behind it."

Stable means production-grade. The bar is not just "tests pass in CI" — it
is **"real users have run this in real clusters and it works."** A feature
reaches Stable when the team has high confidence, informed by real-world
evidence, that it is reliable, observable, documented, and maintainable.

- Feature gate locked to `true` (`LockToDefault: true`)
- API-stable; breaking changes follow a deprecation policy
- Fully documented, observable, and supported

**Minimum soak:** at least **two release cycles at Beta** before promoting.

#### Stable Graduation Requirements

All Beta requirements, plus:

**Real-world validation:**

| # | Requirement | Evidence |
|---|-------------|----------|
| S1 | Two release cycles at Beta with no P0/P1 regressions | Release notes |
| S2 | Real-world user feedback: used in production or production-like environments by at least one user (internal or external) beyond the dev team, and feedback incorporated | GitHub issue, user report, or internal test report |
| S3 | Known edge cases from Beta are fixed or documented with workarounds | Linked issues |

**Testing and reliability:**

| # | Requirement | Evidence |
|---|-------------|----------|
| S4 | Comprehensive BATS: edge cases, failure injection, multi-node (where applicable) | Test + CI links |
| S5 | Scale / stress testing validated under high resource churn and load | Benchmark or test report |
| S6 | K8s compatibility confirmed (current + N-2) via CI matrix | CI matrix evidence |

**Observability and operations:**

| # | Requirement | Evidence |
|---|-------------|----------|
| S7 | Metrics validated: dashboards or alerts can be built from emitted metrics | Grafana JSON or PromQL |
| S8 | Operational runbook or troubleshooting guide | Doc link |

**Finalization:**

| # | Requirement | Evidence |
|---|-------------|----------|
| S9 | No known workarounds for common use cases | Issue tracker review |
| S10 | Feature gate locked (`LockToDefault: true`) | Code link |
| S11 | Gate removal planned (one release after locking, then removed) | Follow-up issue |

### 2.4 Deprecation and Removal

| From Stage | Policy |
|------------|--------|
| Alpha | May be removed in any release without notice |
| Beta | Deprecated for at least **one release** before removal; migration guidance required |
| Stable | Deprecated for at least **two releases**; migration path and tooling if applicable |

When a gate is removed, update `pkg/featuregates/featuregates.go` and
document the removal in the release notes.

### 2.5 Upstream Kubernetes Dependencies

Features that depend on upstream KEPs (e.g., KEP-4815 for DynamicMIG,
KEP-5055 for device taints) must follow this coupling:

| Driver Feature Stage | Minimum Upstream Stage |
|---------------------|----------------------|
| Alpha | Alpha |
| Beta | Beta (or documented fallback) |
| Stable | Stable (strongly preferred) |

When the upstream dependency is not at the required level, the feature must
detect and degrade gracefully, require and fail loudly, or defer promotion.

## 3. Current Feature Gate Inventory

| Feature Gate | Stage | Default | Introduced | Upstream Dependency | Notes |
|-------------|-------|---------|------------|--------------------|----- |
| `TimeSlicingSettings` | Alpha | `false` | v25.8 | — | Custom timeslicing settings |
| `MPSSupport` | Alpha | `false` | v25.8 | — | Multi-Process Service support |
| `IMEXDaemonsWithDNSNames` | Beta | `true` | v25.8 | — | DNS names for IMEX daemons |
| `PassthroughSupport` | Alpha | `false` | v25.12 | — | VFIO-PCI passthrough |
| `DynamicMIG` | Alpha | `false` | v25.12 | [KEP-4815] (Alpha 1.35, Beta target 1.36) | Mutually exclusive with PassthroughSupport, NVMLDeviceHealthCheck, MPSSupport |
| `NVMLDeviceHealthCheck` | Alpha | `false` | v25.12 | [KEP-5055] (Alpha 1.33, Beta target 1.36) | Mutually exclusive with DynamicMIG |
| `ComputeDomainCliques` | Beta | `true` | v25.12 | — | Requires IMEXDaemonsWithDNSNames |
| `CrashOnNVLinkFabricErrors` | Beta | `true` | v25.12 | — | Crash on NVLink errors instead of fallback |

## 4. Current Gaps

This section captures the current gaps before each feature gate can meet its
current stage requirements or graduate to the next stage.

- No metrics endpoint for different components (e.g., `gpu-kubelet-plugin`).
- CI matrix limited to K8s 1.34–1.35.
- No performance baseline.
- Feature gates not documented in README or Wiki.
- No production feedback channel.

## 5. FAQ

**Can a feature skip Beta?**
No. Beta is where the feature is enabled by default and exposed to broad
usage. Skipping it removes the chance to catch issues before locking the gate.

**Who decides when a feature graduates?**
Project maintainers, by reviewing graduation evidence against the criteria
in this document. Evidence is presented in a PR that modifies
`pkg/featuregates/featuregates.go`.

**What if an upstream KEP regresses?**
The driver feature must either implement a fallback or revert to the
previous stability level until the upstream issue is resolved.
