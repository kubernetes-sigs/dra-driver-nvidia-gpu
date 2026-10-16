# AGENTS.md

Guidance for AI coding agents working in this repository. Human contributors should also read [CONTRIBUTING.md](CONTRIBUTING.md) and the [documentation site](https://dra-driver-nvidia-gpu.sigs.k8s.io/), which are the source of truth.

## Project summary

This is the **DRA Driver for NVIDIA GPUs**, a Kubernetes [Dynamic Resource Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) driver. It is a SIG Node subproject (`sigs.k8s.io/dra-driver-nvidia-gpu`) and follows Kubernetes / `client-go` conventions. It manages two kinds of resources, each with its own kubelet plugin:

- **`ComputeDomain`s** (`cmd/compute-domain-controller/`, `cmd/compute-domain-daemon/`, `cmd/compute-domain-kubelet-plugin/`) — an abstraction for robust, secure Multi-Node NVLink (MNNVL). Officially supported. Orchestrates IMEX daemons, domains, and channels under the hood.
- **`GPU`s** (`cmd/gpu-kubelet-plugin/`) — flexible allocation and dynamic reconfiguration of GPUs (sharing, MIG, time-slicing). Officially supported, but some features are still experimental (alpha).

API group: `resource.nvidia.com/v1beta1` (`api/nvidia.com/resource/v1beta1/`). Go module: `sigs.k8s.io/dra-driver-nvidia-gpu` (Go 1.26.x, see [go.mod](go.mod)).

## AI Contribution Policy

This project follows the [Kubernetes AI Tool Usage Policy](https://www.kubernetes.dev/docs/guide/pull-requests/#ai-guidance). Key rules:

- **Disclose AI usage** in the PR description (use `#### Special notes for your reviewer:` in [.github/PULL_REQUEST_TEMPLATE.md](.github/PULL_REQUEST_TEMPLATE.md)).
- **Disclose AI usage** when commenting or filing issues.
- **No AI authorship markers.** Do not add AI co-author lines, `assisted-by`, `co-developed`, or similar commit trailers.
- All PRs still require a signed [CNCF CLA](https://git.k8s.io/community/CLA.md) regardless of how the change was produced.

## Repository layout

| Path | What lives there |
| --- | --- |
| [api/nvidia.com/resource/v1beta1/](api/nvidia.com/resource/v1beta1/) | `ComputeDomain`, `GpuConfig`, `ComputeDomainConfig`, `MigConfig`, `VfioDeviceConfig`, sharing/IOMMU types, and their validation (`validate.go`). Hand-written except `zz_generated.deepcopy.go`. |
| [cmd/gpu-kubelet-plugin/](cmd/gpu-kubelet-plugin/) | GPU kubelet plugin: NVML/nvidia-smi device discovery, MIG, sharing, time-slicing. |
| [cmd/compute-domain-kubelet-plugin/](cmd/compute-domain-kubelet-plugin/) | ComputeDomain kubelet plugin: node-local IMEX channel/domain setup. |
| [cmd/compute-domain-controller/](cmd/compute-domain-controller/), [cmd/compute-domain-daemon/](cmd/compute-domain-daemon/) | Cluster-level ComputeDomain reconciliation and the per-node IMEX daemon. |
| [cmd/webhook/](cmd/webhook/) | Admission webhook validating opaque device config in `ResourceClaim`/`ResourceClaimTemplate` across `resource.k8s.io` v1, v1beta1, v1beta2. |
| [templates/](templates/) | Go `text/template` files rendered at runtime by the controller/kubelet-plugin binaries (e.g. the per-ComputeDomain IMEX DaemonSet, claim templates) — not part of the Helm chart, no `values.yaml` override path. |
| [pkg/nvidia.com/](pkg/nvidia.com/) | **Generated** typed clientset, informers, listers for the `resource.nvidia.com` API group. Do not hand-edit. |
| [pkg/featuregates/](pkg/featuregates/), [pkg/flags/](pkg/flags/) | Feature gate definitions and CLI flag/leader-election/logging helpers shared across binaries. |
| [pkg/imex/](pkg/imex/), [pkg/fabricmanager/](pkg/fabricmanager/) | Clients for IMEX and NVIDIA Fabric Manager, used by the ComputeDomain path. |
| [pkg/metrics/](pkg/metrics/) | Prometheus metric definitions (ComputeDomain cluster state, DRA request counters). |
| [pkg/bootid/](pkg/bootid/), [pkg/flock/](pkg/flock/), [pkg/workqueue/](pkg/workqueue/) | Small shared internals (node boot-ID detection, file locking, a jitter-limited workqueue). |
| [test/e2e/](test/e2e/) | Go/Ginkgo e2e suite (`//go:build e2e`), run via `make test-e2e` against a live cluster with real GPUs. |
| [tests/bats/](tests/bats/) | Bats integration suites against real GPU/MNNVL hardware (`test_gpu_*.bats`, `test_cd_*.bats`). Run via Prow, plus a mock-NVML subset on GitHub Actions ([.github/workflows/mock-nvml-e2e.yaml](.github/workflows/mock-nvml-e2e.yaml)). |
| [deployments/helm/dra-driver-nvidia-gpu/](deployments/helm/dra-driver-nvidia-gpu/) | Helm chart, including the CRDs mirrored under its `crds/` directory. |
| [deployments/devel/](deployments/devel/) | Pinned dev toolchain (golang version, codegen tool versions) and `check-modules`. |
| [hack/](hack/) | Codegen boilerplate, CI scripts, image build/publish scripts. |
| [site/content/](site/content/) | Hugo docs source for https://dra-driver-nvidia-gpu.sigs.k8s.io. `site/content/contribute/proposals/` holds design proposals (this project's KEP-equivalent). |
| [demo/](demo/) | Runnable sample manifests for the kind-based quickstart demo. |

When in doubt about ownership, check [OWNERS](OWNERS).

## Documentation conventions

Follow the [documentation style guide](site/content/contribute/style-guide.md) when writing or reviewing human-facing documentation.
It is the source of truth for prose, page structure, examples, and procedure formatting.
The [documentation contributor guide](site/content/contribute/docs.md) covers local preview and release updates.

## Build, test, lint

All standard tasks go through the [Makefile](Makefile). Prefer `make` targets over invoking tools directly so CI and local runs stay consistent.

- `make check` — `gofmt`/`goimports` diff check, `golangci-lint run ./...`. Run before sending a PR.
- `make build` / `make cmds` / `make binaries` — compile the driver binaries.
- `make test` — `go test -race -cover ./...` (unit tests only; excludes `test/e2e`, which is behind the `e2e` build tag).
- `make test-e2e` — the Ginkgo e2e suite against the current kubectl context; requires a cluster with the driver and real GPUs installed.
- `make bats` / `make bats-gpu` / `make bats-cd` — the bats integration suites; require real GPU/MNNVL hardware, not runnable in a plain dev sandbox.
- `make generate` — regenerates deepcopy, CRDs, clientset, informers, listers (`generate-deepcopy`, `generate-crds`, `generate-clientset`, `generate-informers`, `generate-listers`). `make check-generate` fails CI if generated output is stale.
- `make check-modules` — fails CI if `go.mod`/`go.sum`/`vendor/` are out of sync; also run `make -C deployments/devel check-modules` when the pinned dev toolchain changes.
- `make helm-lint` / `make helm-package` — Helm chart checks.

After editing anything under [api/nvidia.com/resource/v1beta1/](api/nvidia.com/resource/v1beta1/), run `make generate` (or at least `make check-generate` to confirm nothing is stale) and commit the regenerated output alongside the source change.

## Coding conventions

- Idiomatic Go, `client-go`/controller conventions. Controllers and reconcile-style loops must be idempotent and safe under retries.
- Comments explain **why**, not **what**. Identifier names should carry the "what."
- Keep changes scoped to the task. No drive-by refactors, speculative abstractions, or unrelated formatting churn — one concern per PR.
- Every `.go` file starts with the Apache-2.0 boilerplate header ([hack/boilerplate.go.txt](hack/boilerplate.go.txt)), match the existing header exactly rather than inventing a variant.
- Errors: wrap with context (`fmt.Errorf("...: %w", err)`); do not swallow errors silently.
- Logging: use `k8s.io/klog/v2`, not `fmt.Printf`/`log`. Reserve `klog.Infof`/`V(1)` for meaningful state transitions and reserve higher verbosity (`V(4)`+, `V(6)`) for routine/steady-state tracing — match the verbosity level already used by neighboring calls in the file you're editing.
- Imports are grouped with `sigs.k8s.io/dra-driver-nvidia-gpu` as the local prefix (see `goimports -local` in the [Makefile](Makefile) and `local-prefixes` in [.golangci.yaml](.golangci.yaml)); run `make check` rather than hand-formatting import blocks.
- Feature-gate new behavior via [pkg/featuregates/](pkg/featuregates/) (`featuregates.Enabled(featuregates.X)`) rather than ad hoc flags when the behavior is opt-in/experimental — follow the pattern already used in [api/nvidia.com/resource/v1beta1/validate.go](api/nvidia.com/resource/v1beta1/validate.go).
- Gate feature-specific behavior at the call site — `if featuregates.Enabled(featuregates.X) { ... }` around the call, not buried deep inside shared logic (see the pattern throughout [driver.go](cmd/gpu-kubelet-plugin/driver.go)). A function may check the gate internally instead only when it must produce genuinely different behavior either way (e.g. a no-op vs. real work), not as a substitute for gating the call site.
- Metrics: never introduce high-cardinality Prometheus labels (pod names, UIDs, raw error strings), follow the existing metric shapes in [pkg/metrics/](pkg/metrics/).

## API/CRD conventions

- The `resource.nvidia.com/v1beta1` API in [api/nvidia.com/resource/v1beta1/](api/nvidia.com/resource/v1beta1/) is consumed as opaque device config embedded in core `resource.k8s.io` `ResourceClaim`/`ResourceClaimTemplate` objects — it is not a standalone CRD apiserver type in the traditional sense, but it is still versioned and needs backward compatibility within `v1beta1`.
- The admission webhook in [cmd/webhook/](cmd/webhook/) has to keep working across `resource.k8s.io` v1, v1beta1, and v1beta2 simultaneously — when changing validation logic, check all three code paths, not just the newest one.
- Validation lives in [api/nvidia.com/resource/v1beta1/validate.go](api/nvidia.com/resource/v1beta1/validate.go) and friends; add table-driven tests alongside (see `computedomainconfig_test.go`, `sharing_test.go`) rather than only relying on webhook-level or e2e coverage.
- Never hand-edit generated code: `zz_generated.deepcopy.go`, anything under [pkg/nvidia.com/clientset/](pkg/nvidia.com/clientset/), [pkg/nvidia.com/informers/](pkg/nvidia.com/informers/), [pkg/nvidia.com/listers/](pkg/nvidia.com/listers/), or the CRD YAML mirrored into [deployments/helm/dra-driver-nvidia-gpu/crds/](deployments/helm/dra-driver-nvidia-gpu/crds/). Regenerate with `make generate` instead.

## Known regression patterns

Real regressions this driver has hit before, not style preferences.

- **Checkpoint backward compatibility** (`cmd/gpu-kubelet-plugin/checkpointv.go`, same in `cmd/compute-domain-kubelet-plugin/`): the on-disk checkpoint format is versioned and checksummed. A field added to an existing checkpoint version must not become effectively required during (de)serialization, or an older binary's checkpoint fails checksum validation on a newer one (and vice versa). New fields need to stay optional, or land in a new checkpoint version instead. (Background: issue #1080.)
- **`AdminAccess` allocations aren't real device consumption**: an allocation flagged for admin/host access represents monitoring intent, not a workload using the device. Any logic that counts allocated capacity, applies sharing math, or reconfigures a device (MIG, time-slicing, fabric partitioning) based on what's allocated must not treat an admin-access allocation like a normal one, and must reject admin-access against anything other than a full GPU. (`cmd/gpu-kubelet-plugin/device_state.go`)
- **Check in-use before destructive teardown**: with device sharing enabled, more than one prepared claim can reference the same physical GPU or MIG device. Before deleting, resetting, or reconfiguring such a device while tearing down one claim, confirm no other still-prepared claim depends on it. (same file, `cleanup.go`)
- **Static and dynamic MIG are mutually exclusive**: MIG devices are created/destroyed through two different mechanisms depending on a feature gate, and the two aren't interchangeable — code must not assume both are active, or apply one mode's assumptions under the other. Only logic that's genuinely independent of which mode is active should be shared between them. (`cmd/gpu-kubelet-plugin/allocatable.go`, `mig.go`; feature gate `DynamicMIG`)
- **Passthrough (VFIO) driver switches can hang**: a kernel-level bind/unbind for GPU passthrough can get stuck indefinitely (e.g. another process still holding the device open) and become uncancelable. Code issuing a driver switch must stay safe to call again without re-triggering an already-in-flight or already-completed switch, since Prepare/Unprepare get retried and must not pile more attempts onto one that's hung. (`cmd/gpu-kubelet-plugin/vfio-device.go`)
- **Survive process restart, node reboot, and force-deleted pods**: anything the driver persists locally (checkpointed state, in-memory bookkeeping) must stay correct after the plugin process restarts, the node reboots, or a pod is force-deleted mid-operation. On startup, the driver needs a way to tell whether previously-persisted device state is still valid — a real reboot invalidates it entirely — and must reconcile any claim left incomplete against the API server (the authoritative source), not just trust the local checkpoint. (`cmd/gpu-kubelet-plugin/device_state.go`, `cleanup.go`, same pattern in `cmd/compute-domain-kubelet-plugin/`)
- **Prepare/Unprepare concurrency is intentionally restricted**: Prepare and Unprepare calls — including for different claims — are deliberately serialized, not run in parallel; don't assume concurrency, and don't add a competing synchronization mechanism without understanding why fine-grained locking was avoided elsewhere. Separately, a scheduler race or a pod force-deleted while still considered allocated can cause the same device to be claimed twice, or the same claim to be re-prepared while a prior attempt is still in flight — new allocation logic must detect and reject or roll back both. (`cmd/gpu-kubelet-plugin/driver.go`, `device_state.go`; background: [kubernetes/kubernetes#136269](https://github.com/kubernetes/kubernetes/pull/136269))
- **`hostManaged` vs `driverManaged` IMEX mode**: the IMEX daemon's lifecycle can be owned by the driver or by the cluster admin, selected by configuration. Shared code that branches on this mode needs both modes checked/tested — a change validated only against the default is a plausible regression for the other. (`pkg/imex/`, `cmd/compute-domain-kubelet-plugin/`, `cmd/compute-domain-controller/`)
- **`compute-domain-daemon` writes shared objects from every node**: this daemon runs one instance per node in a compute domain, and multiple instances can concurrently create or update the same cluster objects. "Already exists" and "conflict" API errors are an expected consequence of that, not failures — new code touching these objects must handle both gracefully rather than surface them as hard errors. (`cmd/compute-domain-daemon/`)
- **`templates/*.tmpl.yaml` have no Helm override path**: the Go templates rendering dynamically-created cluster objects (e.g. the per-ComputeDomain daemon) aren't exposed as Helm values the way the rest of the deployment is. Any environment-specific requirement added to what they produce (security context, SELinux/SCC needs) needs a real, threaded-through configuration knob, not a hardcoded assumption for one environment. (`templates/`, `cmd/compute-domain-controller/`)

## Testing conventions

- **Unit tests** are co-located `*_test.go` files using `testify` (`require`/`assert`), typically table-driven with a `map[string]struct{...}` of cases and `t.Run(name, ...)`. Run via `make test`. New behavior needs a unit test.
- **E2E tests** live in [test/e2e/](test/e2e/), gated by the `e2e` build tag, and use Ginkgo/Gomega against a real cluster with real GPUs (`make test-e2e`). Follow the existing suite/framework structure in [test/e2e/framework/](test/e2e/framework/) rather than inventing a new harness.
- **Bats integration tests** in [tests/bats/](tests/bats/) exercise the driver end-to-end on real GPU or MNNVL hardware via Prow, split into `gpu` and `cd` (ComputeDomain) suites. [.github/workflows/mock-nvml-e2e.yaml](.github/workflows/mock-nvml-e2e.yaml) runs a subset against mock NVML on CPU-only GitHub Actions runners for fast, cheap coverage — prefer extending that path for logic that doesn't strictly need real silicon.
- When fixing a bug, add a regression test that fails without the fix.

## Pull requests and CI

- This is a Kubernetes SIG project: contributors must have a signed [CNCF CLA](https://git.k8s.io/community/CLA.md). PRs without one are not reviewed.
- CI runs through GitHub Actions ([.github/workflows/](.github/workflows/)) for lint/build/unit tests/mock-NVML e2e, and through Prow for hardware-dependent bats/e2e suites. The `k8s-ci-robot`/Prow bot merges PRs once they have `lgtm` + `approve` and presubmits pass.
- Use the [PR template](.github/PULL_REQUEST_TEMPLATE.md) as-is: `/kind` label, motivation, linked issue, reviewer notes, `release-note` block (write `NONE` if there is no user-facing change), and the pre-flight checklist (`make check test`, `make check-generate` if `api/` changed, `make check-modules` if `go.mod`/`go.sum` changed, Helm chart updates if flags/RBAC/defaults changed).
- Keep PR titles short and imperative; the body should explain motivation ("why"), not just restate the diff.

## Things to avoid

- Do not hand-edit generated files (see "API/CRD conventions" above) — regenerate via `make generate`.
- Do not bypass hooks, skip `gofmt`/`goimports`, or disable lint rules in [.golangci.yaml](.golangci.yaml) to make CI pass — fix the underlying issue.
- Do not commit built binaries, `coverage.out`, or anything the [.gitignore](.gitignore)/[.dockerignore](.dockerignore) already excludes.
- Do not modify [OWNERS](OWNERS) or release automation ([.github/workflows/release-automation.yml](.github/workflows/release-automation.yml), [RELEASE.md](RELEASE.md)) unless the task is explicitly about that.
- Do not assume bats/e2e suites can run in a sandbox without GPUs — they need real hardware (or, for the mock-NVML subset, a kind cluster); say so explicitly rather than claiming a run you couldn't actually do.

## Useful pointers

- Architecture overview and install instructions: [README.md](README.md).
- Docs site (concepts, guides, reference): https://dra-driver-nvidia-gpu.sigs.k8s.io/
- Design proposals: [site/content/contribute/proposals/](site/content/contribute/proposals/).
- Release process: [RELEASE.md](RELEASE.md).
- Slack: `#sig-node` on Kubernetes Slack. New to k8s Slack? Get an invite at https://slack.k8s.io/.
