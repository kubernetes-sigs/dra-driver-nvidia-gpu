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

BUILD_MULTI_ARCH_IMAGES ?= no
DOCKER ?= docker
REGCTL ?= regctl
PUSH_ON_BUILD ?= false
PROVENANCE ?= false
SBOM ?= false

##### Global variables #####
include $(CURDIR)/versions.mk

ifeq ($(IMAGE_NAME),)
IMAGE_NAME := $(REGISTRY)/$(DRIVER_NAME)
endif

# Note: this is sometimes a v-prefixed version string
# (in case of proper releases) or sometimes a commit hash
# (in case of regular CI builds) in which case VERSION
# is not v-prefixed.
IMAGE_VERSION := $(VERSION)

IMAGE_TAG ?= $(IMAGE_VERSION)
IMAGE = $(IMAGE_NAME):$(IMAGE_TAG)

##### Public rules #####
.PHONY: build

# Multi-arch without registry push (e.g. GitHub Actions): BUILD_MULTI_ARCH_GH=true
# writes an OCI layout tarball. Image push to staging/promotion is done by Prow.
DIST_DIR ?= $(CURDIR)/dist
OCI_OUTPUT ?= $(DIST_DIR)/dra-driver-nvidia-gpu.oci.tar

# TODO: revisit compression=zstd,compression-level=15
ifeq ($(BUILD_MULTI_ARCH_IMAGES),true)
BUILDX = buildx
DOCKER_BUILD_PLATFORM_OPTIONS = --platform=linux/amd64,linux/arm64
ifeq ($(BUILD_MULTI_ARCH_GH),true)
DOCKER_BUILD_OPTIONS = \
	--output=type=oci,dest=$(OCI_OUTPUT) \
	--provenance=$(PROVENANCE) \
	--sbom=$(SBOM)
else
DOCKER_BUILD_OPTIONS = \
	--output=type=image,push=$(PUSH_ON_BUILD),oci-mediatypes=true \
	--provenance=$(PROVENANCE) \
	--sbom=$(SBOM)
endif
else
# We also support building single-platform images.
# These are mostly used for local testing and as such we don't allow these to
# be pushed on build.
ifeq ($(PUSH_ON_BUILD),true)
$(error PUSH_ON_BUILD=true is not supported for single architecture builds)
endif
ARCH ?= $(shell uname -m | sed -e 's,aarch64,arm64,' -e 's,x86_64,amd64,')
DOCKER_BUILD_PLATFORM_OPTIONS = --platform=linux/$(ARCH)
endif

DOCKERFILE = $(CURDIR)/deployments/container/Dockerfile

# Use mount cache (on host fs) for Go build&mod cache:
# 1) Local dev: yes (store on host: get all benefits of the Go build cache).
# 2) CI: no (cannot preserve it across builds; store it in container image layer
#    instead for _only some_ of its benefits).
#
# Use the `CI` environment variable to distinguish both cases. Note that
# `/mc/go/build-cache` `/mc/go/pkg/mod` are hard-coded locations in the
# Dockerfile; backed by a mount cache (use those two locations in local dev,
# don't use them in CI).
GOCACHE = /mc/go/build-cache
GOMODCACHE = /mc/go/pkg/mod
ifeq ($(CI), true)
	GOCACHE=/go/build-cache
	GOMODCACHE=/go/pkg/mod
endif

build:
	mkdir -p $(DIST_DIR)
	DOCKER_BUILDKIT=1 \
		$(DOCKER) buildx build --pull \
		$(DOCKER_BUILD_OPTIONS) \
		$(DOCKER_BUILD_PLATFORM_OPTIONS) \
		--tag $(IMAGE) \
		--build-arg GOLANG_VERSION="$(GOLANG_VERSION)" \
		--build-arg TOOLKIT_CONTAINER_IMAGE="$(TOOLKIT_CONTAINER_IMAGE)" \
		--build-arg VERSION="$(VERSION)" \
		--build-arg GIT_COMMIT="$(GIT_COMMIT)" \
		--build-arg GOCACHE="$(GOCACHE)" \
		--build-arg GOMODCACHE="$(GOMODCACHE)" \
		--progress=plain \
		-f $(DOCKERFILE) \
		$(CURDIR)
