APP := scribe
BINDIR := dist
GO ?= go
CONTAINER_ENGINE ?= docker
IMAGE ?= scribe:local

.PHONY: build test race vet verify container verify-container compose-up

build:
	mkdir -p $(BINDIR)
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags='-s -w' -o $(BINDIR)/$(APP) ./cmd/scribe

test:
	$(GO) test ./... -count=1 -timeout 60s

race:
	$(GO) test -race ./... -count=1 -timeout 120s

vet:
	$(GO) vet ./...

verify: test race vet

container:
	$(CONTAINER_ENGINE) build -f Containerfile -t $(IMAGE) .

verify-container:
	CONTAINER_ENGINE="$(CONTAINER_ENGINE)" IMAGE="$(IMAGE)" bash scripts/container-smoke.sh

compose-up:
	$(CONTAINER_ENGINE) compose -f deploy/compose.yaml up --build -d
