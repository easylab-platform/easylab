BINARY := ops-extension
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build frontend test vet fmt check image

## frontend: install deps + build the SPA into frontend/dist
frontend:
	cd frontend && (pnpm install --frozen-lockfile 2>/dev/null || pnpm install) && pnpm build

## build: compile the single binary (embeds frontend/dist)
build: frontend
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

## quick Go-only build (uses whatever is in frontend/dist, placeholder ok)
build-go:
	CGO_ENABLED=0 go build -o bin/$(BINARY) .

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
