---
name: dra-api-crd-feature-gate-change
description: Guide compatible API, CRD, opaque device configuration, ResourceSlice contract, Helm value, kubelet-plugin checkpoint schema, and feature-gate changes in the DRA Driver for NVIDIA GPUs. Use when a user mentions CRDs, API fields, ResourceSlice attributes or capacity, Helm values, checkpoint schemas, feature gates, API evolution, or upgrade and downgrade compatibility.
metadata:
  author: "DRA Driver for NVIDIA GPUs team"
---

# DRA API, CRD, and Feature-Gate Changes

Treat every API field, serialized device property, install value, and persisted checkpoint field as a contract. Implement or review the change from its handwritten source through consumers, generated outputs, upgrade behavior, tests, and documentation.

Do not edit generated code as the source of a change. Preserve unrelated work in a dirty checkout; code generation rewrites generated trees and runs formatting, so inspect `git status` and overlapping changes before invoking it.

## Classify the contract first

Use the narrowest applicable path. A change may span more than one contract:

| Contract | Handwritten source and principal consumers |
|---|---|
| `ComputeDomain` or `ComputeDomainClique` CRD | Types and Kubebuilder markers in `api/nvidia.com/resource/v1beta1/`; registration in `register.go`; controllers and daemons under `cmd/compute-domain-*`; generated CRDs under `deployments/helm/dra-driver-nvidia-gpu/crds/`; generated clients, informers, and listers under `pkg/nvidia.com/`. |
| Opaque device configuration | Config types, defaults, normalization, and validation in `api/nvidia.com/resource/v1beta1/`; decoder registration in `api.go`; consumers in both kubelet plugins and `cmd/webhook/`. |
| ResourceSlice device contract | Device names, attributes, capacity, counters, taints, and publication logic primarily in `cmd/gpu-kubelet-plugin/deviceinfo.go`, `allocatable.go`, MIG/consumable-share helpers, and `driver.go`; ComputeDomain device fields in `cmd/compute-domain-kubelet-plugin/deviceinfo.go` and `driver.go`; DeviceClasses in the Helm templates. |
| Helm value | Default and comments in `deployments/helm/dra-driver-nvidia-gpu/values.yaml`; validation in `_helpers.tpl` or `validation.yaml`; component wiring in the relevant chart templates; reference in `site/content/docs/reference/helm-values.md`. |
| Checkpoint schema | Separate GPU and ComputeDomain schemas under each kubelet plugin's `checkpoint.go`, `checkpointv.go`, prepared-device types, and state logic. These node-local files are not generated. |
| Feature gate | Registry and constraints in `pkg/featuregates/featuregates.go`; shared flag in `pkg/flags/featuregates.go`; binary checks under `cmd/`; Helm propagation; tests and reference docs. |

Before editing, search the exact field, JSON key, gate, attribute, or value name across first-party paths. Exclude `vendor/` and initially exclude generated `pkg/nvidia.com/`; include generated outputs later to verify regeneration.

## Define compatibility before implementation

Write down the current and proposed wire behavior, then evaluate:

- new binary with old objects, claims, Helm values, and checkpoints;
- old binary after the new binary has written state;
- rolling skew between controller, daemon, webhook, and kubelet plugins;
- upgrade and rollback while claims are allocated or Prepare/Unprepare is incomplete;
- clusters with the old CRD while new pods start, and the new CRD while old pods still run;
- gate disabled, enabled, then disabled again;
- missing versus explicit zero/false/empty values;
- supported Kubernetes API versions and required upstream feature gates.

If compatibility requires an assumption, make it explicit and testable. Do not claim a downgrade is supported merely because JSON unmarshalling succeeds; the old binary must preserve checksums, state meaning, defaults, and cleanup behavior.

These contract changes normally require a formal proposal under `site/content/contribute/proposals/`: new or graduated feature gates, user-facing API or CRD fields, Helm values, ResourceSlice attributes, checkpoint schema changes, and significant behavior changes. Read the current proposal policy and template before broad implementation. A narrow bug fix that restores an existing contract may not need a proposal.

## Change CRDs and concrete APIs

For `ComputeDomain` and `ComputeDomainClique` changes:

- Modify the handwritten type and Kubebuilder markers under `api/nvidia.com/resource/v1beta1/`. Choose pointer versus value and `omitempty` based on whether omission must be distinguishable from the zero value and how old readers behave; do not apply either mechanically.
- Preserve existing JSON names and enum meanings. Prefer additive optional fields. Renames, removals, reused enum values, changed defaults, or stricter validation of already-valid stored objects need an explicit migration or a new API version.
- Consider spec immutability, status ownership, list semantics (`+listType` and `+listMapKey`), defaulting, requiredness, bounds, and cross-field validation. Status readers must tolerate fields absent from objects written by older components.
- Update both runtime type registration paths when adding a kind: opaque/runtime decoder registration in `api.go` when applicable, and Kubernetes object scheme registration in `register.go` for concrete resources.
- Trace every writer, reader, informer, indexer, template, RBAC rule, finalizer, and status reconciliation path. Name one authoritative writer for each state field.
- If an incompatible version is necessary, design served/storage versions and conversion before adding it. This checkout has a single `resource.nvidia.com/v1beta1` implementation and no general CRD conversion layer to assume for free.

Regenerate deepcopy code, CRDs, clientsets, informers, and listers with `make generate`. Review generated diffs rather than treating successful generation as proof that the API is compatible.

## Change opaque device configurations

Opaque configs are Kubernetes runtime objects embedded in `ResourceClaim` or `ResourceClaimTemplate` device configuration:

- Define or update types, JSON tags, TypeMeta defaults, `Normalize()`, and `Validate()` in `api/nvidia.com/resource/v1beta1/`. Add new kinds to the decoder scheme in `api.go` and satisfy the package's config interface.
- Keep defaulting deterministic. Decide whether defaults depend on feature gates and whether omission must remain distinguishable from an explicitly supplied value.
- User-provided input is strict-decoded by the optional webhook and again in node preparation paths. Unknown fields must not be accepted merely because the webhook is disabled; validation and feature checks must remain in the binary that applies the config.
- Check configuration precedence. DeviceClass configs precede claim configs, later configs from the same source take precedence, and request-scoped entries override unscoped defaults in the current kubelet-plugin logic.
- Preserve downgrade handling for opaque data recovered through checkpoints. The API package provides a non-strict decoder for paths that must ignore fields introduced by a newer driver; use strict decoding for live user input and non-strict decoding only for an intentional compatibility path.
- Normalize and validate a cloned object before applying it when shared runtime objects might otherwise be mutated.
- Update API unit tests, kubelet-plugin mapping/default/Prepare tests, and webhook tests. The webhook covers both `ResourceClaim` and `ResourceClaimTemplate` and multiple supported `resource.k8s.io` API versions.

## Change ResourceSlice attributes or capacity

ResourceSlice names, attribute keys and types, capacity keys, counter semantics, and device names are scheduler-facing contracts used by CEL selectors, `matchAttribute`, DeviceClasses, tests, scripts, and external automation.

- Do not silently rename a key or change its scalar/list/value type. Add a new field, retain the old representation for a compatibility period, or gate a representation change with clear Kubernetes-version requirements.
- Use standard qualified attributes from upstream helpers where appropriate; otherwise confirm how the driver name qualifies unqualified map keys in serialized ResourceSlices and user selectors.
- Trace discovery through `GetDevice()`/attribute/capacity builders into `GenerateDriverResources()` and publication. Account for full GPU, static and dynamic MIG, VFIO, split versus combined ResourceSlices, health taints, and consumable capacity as applicable.
- Treat canonical device names with extra care: they return through allocation results, are looked up during Prepare, and may appear in checkpoints and user-visible errors.
- Update relevant Helm DeviceClass selectors, demo claims, `site/content/docs/reference/resourceslice-attributes.md`, concept or guide pages, unit tests, and ResourceSlice-aware e2e tests.
- Verify absence behavior on unsupported hardware. Do not publish misleading zero or empty attributes when discovery cannot establish a value unless the contract explicitly defines that representation.

## Change Helm values

- Add or modify the default in `values.yaml` and wire it to every affected component. A documented value that no template consumes is not implemented.
- Fail early in Helm helpers or `validation.yaml` for combinations the chart can determine. Retain binary-side validation for configurations that can bypass Helm or depend on runtime state.
- Check enablement conditions, environment variables, args, volumes, mounts, RBAC, ServiceAccounts, DeviceClasses, security contexts, and dynamic template data. Values used by multiple binaries must not create unsafe skew.
- Define upgrade semantics for renamed, deprecated, or changed defaults. Prefer a compatibility alias or explicit migration over silently reinterpreting an existing value.
- Update `site/content/docs/reference/helm-values.md`, installation/upgrade guidance, examples, and tests. Use `helm lint` and targeted `helm template` cases for both valid and invalid combinations.

## Change checkpoint schemas

GPU and ComputeDomain kubelet plugins own independent node-local checkpoints. Inspect both before assuming a shared schema change.

- Preserve the version envelope, checksum behavior, and conversion functions in `checkpoint.go` and `checkpointv.go`. The current writers retain a legacy representation alongside the latest version to support downgrade.
- Add `omitempty` to new checkpoint fields unless emitting a zero value is deliberately part of the old wire contract. Be especially careful when serializing upstream structs: a newly added upstream field without `omitempty` can change the bytes covered by an older checksum.
- Add a new checkpoint version when state meaning or conversion cannot be represented safely as an additive field. Define old-to-new and, when downgrade is supported, new-to-old conversion. Decide what newer-only or partially prepared state becomes on downgrade.
- Test missing fields, unknown newer fields, both checksum versions, old-to-new conversion, new write followed by old read, corrupted data, node reboot, plugin restart, PrepareStarted/PrepareCompleted/PrepareAborted states where applicable, and cleanup of stale claims.
- Exercise live upgrade and downgrade in `tests/bats/test_gpu_updowngrade.bats` or the corresponding ComputeDomain coverage when unit tests cannot prove the lifecycle.

Never “repair” a compatibility failure by ignoring checksum or corruption errors without a design that preserves the checkpoint's source-of-truth role.

## Change feature gates

- Add the gate constant and `VersionedSpecs` entry in `pkg/featuregates/featuregates.go`. Driver-local `Version` fields use driver SemVer major/minor; `featureGateEmulationVersion` instead tracks the vendored Kubernetes minor.
- Follow existing lifecycle defaults unless the proposal justifies otherwise: Alpha normally defaults off and Beta normally defaults on. State exact disabled and enabled behavior.
- Add dependency and mutual-exclusion checks to `ValidateFeatureGates()` and its table-driven tests. Validate in every binary that can receive the configuration.
- Trace Helm's `FEATURE_GATES` propagation to controller, daemon, both kubelet plugins, and webhook, but expose a gate only to components that can behave safely under rolling skew.
- Define the interaction with persisted checkpoints, CRDs, generated templates, ResourceSlices, active claims, rollback, required Kubernetes gates/API versions, NVIDIA driver versions, hardware, and external services.
- Update the feature-gate reference, relevant guides/API docs, Helm examples or validation, release notes, `.github/ISSUE_TEMPLATE/bug_report.yml`, and issue-triage gate lists when present.
- For graduation, document feature completeness, named interoperability combinations, stability evidence, and soak time. Do not graduate based only on changing `PreRelease` and the default.

## Verify the complete change

Use targeted tests while iterating, then verify in proportion to the contract:

- API/defaulting/validation: package tests plus affected plugin and webhook tests.
- Generated API/CRD: during active edits, run `make generate` and inspect all generated diffs. Run `make check-generate` only from a clean committed snapshot or disposable clean checkout containing the intended source and generated changes.
- Feature gates: run `go test` on the `pkg/featuregates` and `pkg/flags` packages, plus every consuming component's tests.
- Helm: `make helm-lint` and targeted render/failure cases.
- General Go change: `make test`; run `make check` for the full lint and generation gate under the same clean-tree conditions as `make check-generate`.
- ResourceSlice, checkpoint, or hardware lifecycle: the matching Go e2e, mock-NVML, BATS, or real-GPU/ComputeDomain scenario. Read `tests/bats/README.md` before running the invasive live-cluster suite.

Both `make check-generate` and `make check` regenerate files and compare the whole tree against `HEAD` with `git diff --exit-code HEAD`. Expected uncommitted source, generated, or unrelated changes therefore cause a diff failure even when generation is correct. Preserve unrelated work and do not report that expected dirty-tree diff as a code-generation failure.

Review the final diff for handwritten source, generated outputs, deployment wiring, docs, examples, and tests. Report the compatibility matrix, generated artifacts, validation commands, and any upgrade or rollback case that was not exercised.
