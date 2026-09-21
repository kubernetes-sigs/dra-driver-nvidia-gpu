**Warning:** the test suite runs _invasively_ against the Kubernetes cluster that your local `kubectl` is currently configured against.

# Usage

General instructions:

1) Review the `TEST_*` variables [at around the top of the Makefile](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/main/tests/bats/Makefile#L22). Most of them can be overridden via environment.
Use this configuration interface to customize your test run.

2) Invoke `make bats` in the root of the repository.

Two examples are shown below.

## Test local dev state (artifacts not pushed)

**Step 1:** Build container image and push to Kubernetes nodes:
```
$ make image-build-and-copy-to-nodes
...
#37 writing image sha256:9529200b5a5e7a3e09340fc9a1ef5c1058453b3f1a03ddb2bec0fe69efba944c done
#37 naming to registry.k8s.io/dra-driver-nvidia/dra-driver-nvidia-gpu:v25.12.0-dev done
...
internal IPs of k8s nodes:
10.115.19.11
10.115.19.12
10.115.19.13
10.115.19.14
export image ...
export/compress took: 1.87 s
-rw------- 1 jgehrcke jgehrcke 78M Nov 27 16:22 ./dra-driver-dev-img.LMd4LU2.tar.gz
push image to nodes ...
...
unpacking registry.k8s.io/dra-driver-nvidia/dra-driver-nvidia-gpu:v25.12.0-dev (sha256:b49decbd5e0dfccb96e0c48727ea19ae59aa4ac475630196ff83c72c53bcc7e6)...done
copy/import(10.115.19.14) took: 2.53 s
done
```

**Step 2:** Start test suite
```
$ TEST_CHART_LOCAL=1 make bats
make -f tests/bats/Makefile tests
...
test_basics.bats
...
 ✓ confirm no kubelet plugin pods running [168]
...
```

This installs the  Helm chart currently specified in `deployments/helm/dra-driver-nvidia-gpu` in the local checkout, and expects it to point to the container image spec used for pushing above's image to the nodes (`registry.k8s.io/dra-driver-nvidia/dra-driver-nvidia-gpu:v25.12.0-dev` in the example above).

Note that `TEST_CHART_LOCAL=1` just overrides `TEST_CHART_REPO` and `TEST_CHART_VERSION`.

`make image-build-and-copy-to-nodes` is just an opinionated helper that expects a certain environment.
If that does not work for you: make sure (out-of-band) that the container images that the local chart refers to are available to all nodes in the Kubernetes cluster -- placed directly or pullable.

To test a different image, provide its complete tagged reference. For an image
preloaded into the container runtime on every node, prevent registry access with
`TEST_IMAGE_PULL_POLICY=Never`:

```console
$ TEST_CHART_LOCAL=1 \
    TEST_IMAGE=custom_registry:custom_tag \
    TEST_IMAGE_PULL_POLICY=Never \
    make -f tests/bats/Makefile tests-gpu-dynmig
```

Use `IfNotPresent` instead when the registry is reachable and the node may need
to pull the image. The override applies to the chart under test, not to the
last-stable chart used by upgrade and downgrade tests.

For a private registry, create the pull secret in the
`dra-driver-nvidia-gpu` namespace before starting the suite and pass its name
with `TEST_IMAGE_PULL_SECRETS`. Multiple secret names can be separated by
commas:

```console
$ TEST_CHART_LOCAL=1 \
    TEST_IMAGE=custom_registry:custom_tag \
    TEST_IMAGE_PULL_SECRETS=registry-credentials,registry-mirror-credentials \
    make -f tests/bats/Makefile tests-gpu-dynmig
```

The secrets are added only to the chart under test, like the image and pull
policy overrides. The BATS runner references existing Kubernetes secrets; it
does not create or copy registry credentials.

## Test staging chart/image artifacts

Example:

```console
$ export TEST_CHART_VERSION="25.8.0-dev-b823882b-chart"
$ make bats
...
12 tests, 0 failures in 166 seconds
```

Note: by default, the test suite assumes a Helm chart exists at `oci://gcr.io/k8s-staging-nvidia/charts/dra-driver-nvidia-gpu` (after Prow promotion flow), with images from staging or `registry.k8s.io/dra-driver-nvidia/dra-driver-nvidia-gpu`.


### Defaults

By default, `make bats` tries to install a Helm chart from `oci://gcr.io/k8s-staging-nvidia/charts/dra-driver-nvidia-gpu` corresponding to the git revision of the local checkout:

```console
$ git rev-parse --short=8 HEAD
e6e1dde4
$ make bats
...
        --env TEST_CHART_REPO="oci://gcr.io/k8s-staging-nvidia/charts/dra-driver-nvidia-gpu" \
        --env TEST_CHART_VERSION=25.8.0-dev-e6e1dde4-chart \
        --env TEST_CRD_UPGRADE_TARGET_GIT_REF=e6e1dde4 \
...
```

That's CI-oriented.


## Resources for development

Bats is a workable solution.
Developing new tests might however probe your patience.
Make wise usage of

* bats' [`run`](https://bats-core.readthedocs.io/en/stable/writing-tests.html#run-test-other-commands) command.
* [skipping tests](https://bats-core.readthedocs.io/en/stable/writing-tests.html#skip-easily-skip-tests)
* [tagging tests with `bats:focus`](https://bats-core.readthedocs.io/en/stable/writing-tests.html#special-tags)
* [CLI args](https://bats-core.readthedocs.io/en/stable/usage.html) such as `--verbose-run`, `--show-output-of-passing-tests`.

Other references:
* https://github.com/bats-core/bats-assert
* https://github.com/bats-core/bats-file


Misc notes:

* The test suite stops on first failure (using the [new](https://github.com/bats-core/bats-core/issues/209) `--abort` flag for bats).
  The tests are not perfectly independent yet, and hence that's sane default behavior.
* Don't skip the section about when [not to use `run`](https://bats-core.readthedocs.io/en/stable/writing-tests.html#when-not-to-use-run).
* Take inspiration from [cri-o tests](https://github.com/cri-o/cri-o/tree/81e69a58c7e6ec8699b3bdd8696b1d0e25e32bfb/test).
* We can and should radically iterate on the test suite's config interface to satisfy our needs.
