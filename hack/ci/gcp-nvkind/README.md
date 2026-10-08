# hack/ci/gcp-nvkind

Prow-driven e2e harness: acquires a Boskos project, provisions a T4 GCE VM,
stands up a DRA-enabled nvkind cluster, installs GPU Operator (minimal
mode) + the DRA driver from source, then runs the Ginkgo suite under
`test/e2e/`.

## Layout

```
hack/ci/gcp-nvkind/
├── README.md
├── e2e-test.sh                 # outer orchestrator (Prow entry point)
├── install-gpu-operator.sh     # helm install, driver/toolkit/devicePlugin off, CDI on
├── install-dra-driver.sh       # docker build + kind load + helm install
├── collect-artifacts.sh        # on-VM postmortem dump
└── lib/                        # generic helpers, kept in sync with
    ├── boskos.sh               # k8s.io/test-infra experiment/gcp-nvkind/lib/;
    ├── gce.sh                  # e2e-test.sh prefers that copy when
    ├── setup-nvkind-node.sh    # TESTINFRA_DIR is set (Prow), falls back
    └── nvkind-config.yaml.tmpl # to this one for local runs
```

## Local run

Requires `gcloud` authed with Compute IAM on the target project, plus SSH
keys in `~/.ssh/google_compute_engine`.

```bash
export GCP_PROJECT=my-gcp-project   # skips Boskos
export GCE_ZONE=us-central1-b
export JOB_NAME=local
export BUILD_ID=$(date +%s)
export ARTIFACTS=/tmp/dra-artifacts
make e2e-gcp-nvkind
```

## Prow run

`dra-driver-nvidia-gpu-gcp-nvkind.yaml` in `kubernetes/test-infra` provides
the full env. `TESTINFRA_DIR` points `e2e-test.sh` at the shared lib under
`experiment/gcp-nvkind/lib/` instead of the in-repo copy.

## Notable pins and caveats

- `registry.k8s.io/dra-driver-nvidia/dra-driver-nvidia-gpu:v0.4.0-dev` is
  not published; `install-dra-driver.sh` builds from source and
  `kind load docker-image`s the result.
- `kindest/node:v1.37.0`. Device health status (`ResourceHealthStatus`) is
  beta and on by default since 1.36, so no extra kubelet feature gate is
  needed to exercise it.
- kind `v0.33.0`, and nvkind rebuilt against it: Kubernetes 1.37 requires the
  kubeadm v1beta4 config that kind v0.32.0+ generates. See
  `lib/setup-nvkind-node.sh`.
- GPU Operator `v26.3.1` in minimal mode: `driver`, `toolkit`, and
  `devicePlugin` disabled; `cdi.enabled=true`, `nfd.enabled=true`.
- DLVM ships the NVIDIA driver and toolkit but not Docker/Go/kind/helm/kubectl;
  `setup-nvkind-node.sh` installs the rest.
- `accept-nvidia-visible-devices-as-volume-mounts` is off by default even on
  DLVM; `setup-nvkind-node.sh` turns it on (required by nvkind).
