# syntax=docker.io/docker/dockerfile:1.7-labs
# The above enables the COPY --exclude + --parent flags
# Copyright The Kubernetes Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Declared here for use in FROM directives below
ARG TOOLKIT_CONTAINER_IMAGE=unknown

# Run build with binaries native to the current build platform.
FROM --platform=$BUILDPLATFORM nvcr.io/nvidia/base/ubuntu:jammy-20260217 AS build

# Require arg to be provided (set invalid default value).
ARG GOLANG_VERSION=x.x.x

# Controlled by Makefile: location(s) of Go build/mod cache. CI: in image layer.
# Local dev: mount cache (host filesystem). ARGs are available as environment
# variables in RUN steps of the same stage (relied upon below).
ARG GOCACHE
ARG GOMODCACHE

# BUILDARCH, TARGETARCH (and others) are defined in the global scope by
# BuiltKit. BUILDARCH is the architecture of the build platform. TARGETARCH is
# set via the --platform arg provided to the `docker buildx build ...` command.
# Redefining those variables here without new values makes the outer-context
# values available to in-stage RUN commands. Arch values are of the form
# amd64/arm64.
ARG BUILDARCH
ARG TARGETARCH

# Dependencies for Go build. Clear apt cache; keep this layer small.
RUN apt-get update && apt-get install -y \
        curl time make \
        gcc \
        gcc-aarch64-linux-gnu \
        gcc-x86-64-linux-gnu && \
        rm -rf /var/lib/apt/lists/* /var/cache/apt/archives

RUN curl -sSfL --retry 8 --retry-all-errors --connect-timeout 10 --retry-delay 5 \
        https://go.dev/dl/go${GOLANG_VERSION}.linux-${BUILDARCH}.tar.gz | tar -C /usr/local -xz

ENV GOPATH=/go
ENV PATH=$GOPATH/bin:/usr/local/go/bin:$PATH
WORKDIR /build

# Create a layer that only invalidates when _rarely_ changing Go code changes
# (`vendor`, `pkg`, ..). Goal: useful Go build&mod cache in an image layer (for
# CI). The mount cache below is only used in local dev (ignored in CI because
# GO[MOD]CACHE does not point to these paths).
# https://github.com/moby/buildkit/blob/master/frontend/dockerfile/docs/reference.md#example-cache-go-packages
COPY go.mod go.sum ./
COPY --parents pkg api internal vendor ./
RUN --mount=type=cache,target=/mc/go/build-cache,id=gocache \
    --mount=type=cache,target=/mc/go/pkg/mod,id=gomodcache \
    case "$TARGETARCH" in "$BUILDARCH") cc=gcc;; arm64) cc=aarch64-linux-gnu-gcc;; amd64) cc=x86_64-linux-gnu-gcc;; esac && \
    CC=${cc} CGO_ENABLED=1 GOARCH=${TARGETARCH} time go build -mod=vendor ./...

# Copy everything else (and only) what is required for the Go build ('Explicit
# include' philosophy). Goal: invalidate layer only when absolutely required.
RUN mkdir /build/deployments /build/hack
COPY common.mk versions.mk Makefile ./
COPY deployments/devel deployments/devel
COPY hack/golang-version.sh hack/golang-version.sh
COPY hack/toolkit-container-image.sh hack/toolkit-container-image.sh
COPY cmd ./cmd

# The vars VERSION & GIT_COMMIT are consumed (as environment variables) by the
# `make` target below. That implies that the cached image layer is invalidated
# for every commit. Hence, during local dev, there's great benefit from backing
# the Go build/mod cache by a mount cache.
ARG VERSION="N/A"
ARG GIT_COMMIT="unknown"
RUN mkdir /artifacts
RUN --mount=type=cache,target=/mc/go/build-cache,id=gocache \
    --mount=type=cache,target=/mc/go/pkg/mod,id=gomodcache \
    case "$TARGETARCH" in "$BUILDARCH") cc=gcc;; arm64) cc=aarch64-linux-gnu-gcc;; amd64) cc=x86_64-linux-gnu-gcc;; esac && \
    time make CC=${cc} GOARCH=${TARGETARCH} PREFIX=/artifacts cmds

# Static bash binary, sourced from the Debian 'bash-static' package on
# trixie. Replaces a from-source build that was flaky in CI (autoconf
# availability, GNU mirror instability, cross-compile segfaults; see
# issues/502 and issues/743) and dominated CI duration on the happy
# path. Pinned by image digest for reproducibility; bash version follows
# the trixie suite. Currently delivers bash 5.2.37, statically linked.
FROM debian:trixie-slim@sha256:8d7a3dca57e62717b0f10897aca189da5d7acde3cc1ced657bdfd06ef5379576 AS bash
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash-static \
 && rm -rf /var/lib/apt/lists/* \
 && /bin/bash-static --version

# Fabric Manager shared library (libnvfm.so), required at runtime by the
# gpu-kubelet-plugin to talk to nv-fabricmanager on NVSwitch-based HGX nodes.
# The distroless runtime image has no package manager, so install the FM SDK
# package here and copy the resulting library into the final image below.
FROM nvcr.io/nvidia/base/ubuntu:jammy-20260217 AS fabricmanager
# Install the FM SDK and stage libnvfm.so (+ versioned soname) into an
# arch-neutral directory. The package installs into the target arch's multiarch
# dir (e.g. x86_64-linux-gnu or aarch64-linux-gnu), so we normalize the location
# here and copy with -a to preserve the symlink chain.
RUN apt-get update \
 && apt-get install -y --no-install-recommends nvidia-fabricmanager-dev-580 \
 && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives \
 && mkdir -p /fm-lib \
 && cp -a /usr/lib/*-linux-gnu/libnvfm.so* /fm-lib/

# Pull the nvidia-cdi-hook binary out of the relevant toolkit container
# (arch: TARGETPLATFORM, set via --platform).
FROM ${TOOLKIT_CONTAINER_IMAGE} AS toolkit

# Construct production image (arch: TARGETPLATFORM, set via the `--platform` CLI
# arg). gcr.io/distroless/cc:debug is distroless/cc plus busybox at /busybox/
# (PATH includes /busybox). The scripts bundled in this image (kubelet-plugin-
# prestart.sh, bind_to_driver.sh, unbind_from_driver.sh) require coreutils
# (ln, find, date, sleep, head, env, readlink, basename) provided by busybox.
FROM gcr.io/distroless/cc:debug

ENV NVIDIA_DISABLE_REQUIRE="true"
ENV NVIDIA_VISIBLE_DEVICES=all
ENV NVIDIA_DRIVER_CAPABILITIES=utility
ARG VERSION="N/A"
ARG GIT_COMMIT="unknown"

LABEL io.k8s.display-name="DRA Driver for NVIDIA GPUs"
LABEL name="DRA Driver for NVIDIA GPUs"
LABEL vendor="NVIDIA"
LABEL version=${VERSION}
LABEL com.nvidia.git-commit="${GIT_COMMIT}"
LABEL release="N/A"
LABEL summary="DRA Driver for NVIDIA GPUs"
LABEL description="DRA Driver for NVIDIA GPUs"
LABEL org.opencontainers.image.description="DRA Driver for NVIDIA GPUs"
LABEL org.opencontainers.image.source="https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu"

# Add top-level license (AL2) file into the container image
COPY LICENSE /

COPY --from=bash    /bin/bash-static                          /bin/bash

# Fabric Manager SDK shared library (and its versioned soname) so the
# gpu-kubelet-plugin can dlopen libnvfm.so at runtime. Placed in /usr/lib so the
# path is arch-neutral and resolvable via an absolute dlopen.
COPY --from=fabricmanager /fm-lib/ /usr/lib/

COPY --from=toolkit /artifacts/rpm/usr/bin/nvidia-cdi-hook    /usr/bin/nvidia-cdi-hook
COPY --from=build   /artifacts/compute-domain-controller      /usr/bin/compute-domain-controller
COPY --from=build   /artifacts/compute-domain-kubelet-plugin  /usr/bin/compute-domain-kubelet-plugin
COPY --from=build   /artifacts/compute-domain-daemon          /usr/bin/compute-domain-daemon
COPY --from=build   /artifacts/gpu-kubelet-plugin             /usr/bin/gpu-kubelet-plugin
COPY --from=build   /artifacts/webhook                        /usr/bin/webhook

COPY /hack/kubelet-plugin-prestart.sh                         /usr/bin/kubelet-plugin-prestart.sh
COPY /templates /templates

# Use root by default (for example, the init container as of now needs
# this, otherwise `ln: /driver-root: Permission denied`).
COPY --from=build /etc/passwd /etc/passwd
COPY --from=build /etc/group /etc/group
USER root:root

# Smoke-test executables (provide early build feedback).
RUN ["/usr/bin/compute-domain-kubelet-plugin", "--version"]
RUN ["/bin/bash", "--version"]
