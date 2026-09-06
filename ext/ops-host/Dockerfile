# syntax=docker/dockerfile:1
# ops-extension: single binary with embedded Svelte SPA, built from this repo's
# own directory as the build context (no shared parent context). The Go SDK
# dependency is resolved from go.mod via jjlab GOPROXY, not copied from a
# sibling directory.
ARG REGISTRY=docker.io
ARG NODE_IMAGE=26-alpine
ARG GO_IMAGE=golang:1.26-alpine

FROM ${REGISTRY}/library/node:${NODE_IMAGE} AS frontend
ARG HTTP_PROXY=http://mihomo.develop.svc.cluster.local:7890
ARG HTTPS_PROXY=http://mihomo.develop.svc.cluster.local:7890
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    NO_PROXY=localhost,127.0.0.1,.svc.cluster.local,.svc
WORKDIR /fe
COPY frontend/package.json frontend/pnpm-lock.yaml ./
RUN npm install -g pnpm && (pnpm install --frozen-lockfile || pnpm install)
COPY frontend/ ./
RUN pnpm build

FROM ${REGISTRY}/library/${GO_IMAGE} AS build
ARG HTTP_PROXY=http://mihomo.develop.svc.cluster.local:7890
ARG HTTPS_PROXY=http://mihomo.develop.svc.cluster.local:7890
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    NO_PROXY=localhost,127.0.0.1,.svc.cluster.local,.svc \

    GOPROXY=http://jj-lab.temp.svc.cluster.local/pkgs/go|direct \
    GOSUMDB=off \
    GONOSUMDB=abep.dev/sdk,abep.dev/sdk/nats,abep.dev/sdk/ws \
    GOFLAGS=-mod=mod
RUN sed -i 's|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g' /etc/apk/repositories \
    && apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
COPY --from=frontend /fe/dist /src/frontend/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/ops-extension .

FROM scratch
COPY --from=build /out/ops-extension /ops-extension
ENTRYPOINT ["/ops-extension"]
