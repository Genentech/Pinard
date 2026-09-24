SHELL := /bin/bash

PROJECT_NAME = pinard-solo
# Repo root (two levels up from build/pinard-solo).
REPO_ROOT = ../..

NATS_VERSION  = $(shell cat ${REPO_ROOT}/.nats-server-version | tr -d '[:space:]')
ENGRAM_VERSION = $(shell cat ${REPO_ROOT}/.engram-version | tr -d '[:space:]')
SURREAL_VERSION = $(shell cat ${REPO_ROOT}/.surreal-version | tr -d '[:space:]')

ARCH = amd64
# In CI, IMAGE is overridden by the CI job variable (${CI_REGISTRY_IMAGE}/pinard-solo).
# For local builds, override IMAGE on the command line or set it here.
IMAGE ?= pinard-solo
IMAGE_TAG ?= latest

NATS_URL   = https://github.com/nats-io/nats-server/releases/download/v${NATS_VERSION}/nats-server-v${NATS_VERSION}-linux-${ARCH}.zip
ENGRAM_URL = https://github.com/Gentleman-Programming/engram/releases/download/v${ENGRAM_VERSION}/engram_${ENGRAM_VERSION}_linux_${ARCH}.tar.gz
SURREAL_URL = https://github.com/surrealdb/surrealdb/releases/download/v${SURREAL_VERSION}/surreal-v${SURREAL_VERSION}.linux-${ARCH}.tgz

# ── Fetch binaries onto the host ─────────────────
# The docker build sandbox has no direct github egress; fetch here then COPY in.

# Binaries are fetched into build/ (relative to this Makefile, i.e.
# build/pinard-solo/build/ relative to repo root) and COPY'd in
# by the Dockerfile using the repo-root build context.
BUILD_DIR = build

${BUILD_DIR}/nats-server:
	mkdir -p ${BUILD_DIR}
	wget -q -O /tmp/nats-server.zip "${NATS_URL}"
	unzip -jo /tmp/nats-server.zip "*/nats-server" -d ${BUILD_DIR}/
	chmod +x ${BUILD_DIR}/nats-server
	rm /tmp/nats-server.zip

${BUILD_DIR}/engram:
	mkdir -p ${BUILD_DIR}
	curl -fsSL "${ENGRAM_URL}" | tar -xz -C ${BUILD_DIR} engram
	chmod +x ${BUILD_DIR}/engram

${BUILD_DIR}/surreal:
	mkdir -p ${BUILD_DIR}
	wget -q -O /tmp/surreal.tgz "${SURREAL_URL}"
	tar -xz -C ${BUILD_DIR} -f /tmp/surreal.tgz surreal
	chmod +x ${BUILD_DIR}/surreal
	rm /tmp/surreal.tgz

fetch-binaries: ${BUILD_DIR}/nats-server ${BUILD_DIR}/engram ${BUILD_DIR}/surreal

# Run docker build from the repo root so all COPY paths resolve correctly.
# The Dockerfile path is relative to the repo root.
image: fetch-binaries
	cd ${REPO_ROOT} && docker build \
		-f build/pinard-solo/Dockerfile \
		-t ${IMAGE}:${IMAGE_TAG} \
		.

image-push: image
	docker push ${IMAGE}:${IMAGE_TAG}

.PHONY: fetch-binaries image image-push
