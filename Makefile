BINARY := ops-extension
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build frontend test vet fmt check image

## build: compile the single binary (no embedded SPA)
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: fmt vet test

## image: build the container image (expects the shared-parent Dockerfile context)
image:
	podman build -f Dockerfile -t zergx-ops-extension:dev ..
