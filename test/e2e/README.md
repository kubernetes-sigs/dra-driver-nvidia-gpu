# test/e2e

End-to-end suite for `dra-driver-nvidia-gpu` using Go + Ginkgo v2. Covers eight
DRA allocation scenarios driven by CEL selectors and embedded YAML templates.

## Coverage

| # | Test | What it validates |
|---|---|---|
| 1 | `[install]` | `gpu.nvidia.com` driver publishes a ResourceSlice with productName / driverVersion / memory. |
| 2 | `[cel/productName]` | CEL regex on `productName` schedules a pod onto a matching GPU. |
| 3 | `[cel/driverVersion]` | CEL semver `compareTo(semver(X)) >= 0` on `driverVersion`. |
| 4 | `[cel/memory]` | CEL quantity compare on `capacity.memory` at a 90% threshold. |
| 5 | `[sharing]` | N pods share a single `ResourceClaim` (time-slicing outcome). |
| 6 | `[negative]` | Unmatchable selector leaves the pod Pending with no allocation. |
| 7 | `[consumable-shares/unlimited]` | Multiple pods share the same GPU concurrently with `--consumable-shares=unlimited`. |
| 8 | `[consumable-shares/memory]` | Multiple pods with explicit fractional memory requests share the same GPU with `--consumable-shares=memory`. |

The suite detects GPU product / driver / memory from the published
`ResourceSlice` at `BeforeSuite`, so it adapts to whatever hardware the CI
harness provisions (T4, L4, A10, A100, H100, etc.).

## Prerequisites

- Kubernetes cluster with `DynamicResourceAllocation` enabled.
- GPU Operator installed (minimal mode is fine) so node-feature-discovery
  labels the node with `nvidia.com/gpu.product`, `nvidia.com/gpu.memory`,
  etc.
- DRA driver installed (`dra-driver-nvidia-gpu` chart or equivalent).
- `kubectl` on PATH; current context pointing at the target cluster.

## Running

```bash
make test-e2e                                   # from the repo root
make test-e2e-gpu-workloads                     # one full GPU for one pod
make test-e2e-static-mig                        # one preconfigured MIG device
make test-e2e-full-gpu                          # full GPU basic workloads
make test-e2e E2E_LABEL_FILTER='full-gpu && multi-gpu' # distinct GPUs for two pods
make test-e2e E2E_LABEL_FILTER='fastfeedback && !mig' # fast feedback excluding MIG
make test-e2e E2E_FOCUS='\[cel/memory\]'         # select by Ginkgo name regex
make test-e2e-static-mig E2E_ARGS='-ginkgo.dry-run' # preview selection
go test -mod=vendor -tags=e2e -v -timeout=30m ./test/e2e/... -ginkgo.v   # directly
```

The generic `test-e2e-<label>` target sets `E2E_LABEL_FILTER` to `<label>`.
It works for any existing or newly added label without another Makefile target.
`E2E_LABEL_FILTER` accepts Ginkgo label expressions with `&&`, `||`, `!`,
and parentheses. Labels on a `Describe` apply to every spec inside it.
Add `Label("your-label")` to an `It` to select that individual spec, or to a
`Describe` to select a group. With no filter, `make test-e2e` runs the whole
suite. If both `E2E_FOCUS` and `E2E_LABEL_FILTER` are set, specs must match both.

`$ARTIFACTS/junit_01.xml` is produced by `-ginkgo.junit-report`, which Prow
picks up automatically when `runner.sh` sets `ARTIFACTS`.

## Layout

```
test/e2e/
├── README.md
├── suite_test.go              # Ginkgo bootstrap + GPU detection
├── gpu_allocation_test.go     # 8 It() specs
└── framework/
    ├── client.go              # k8s client factory
    ├── gpu.go                 # ResourceSlice -> GPUDetails
    ├── manifests.go           # embed + render specs/*.tmpl
    ├── wait.go                # pod/claim polling helpers
    └── specs/                 # embedded YAML templates
        ├── consumable-shares-memory.yaml.tmpl
        ├── consumable-shares-unlimited.yaml.tmpl
        ├── driver-version.yaml.tmpl
        ├── error-handling.yaml.tmpl
        ├── memory-size.yaml.tmpl
        ├── product-type.yaml.tmpl
        └── timeslicing.yaml.tmpl
```
