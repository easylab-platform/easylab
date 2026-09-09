# syntax=docker/dockerfile:1
# ops-extension: single Go binary (thin agent glue layer; no embedded SPA —
# the frontend moved to easylab as the single gateway entry). Built from this
# repo's own directory as the build context. Modules are vendored so the
# build resolves nothing from the network for Go deps (easylab-proto's
# worker/v1 types etc. ship in vendor/).
ARG REGISTRY=forgejo.develop.10.199.64.20.nip.io/root
ARG GO_IMAGE=golang:1.26-alpine

FROM ${REGISTRY}/${GO_IMAGE} AS build
ARG HTTP_PROXY=http://mihomo.develop.svc.cluster.local:7890
ARG HTTPS_PROXY=http://mihomo.develop.svc.cluster.local:7890
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    NO_PROXY=localhost,127.0.0.1,.svc.cluster.local,.svc \
    GOFLAGS=-mod=vendor
RUN sed -i 's|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g' /etc/apk/repositories \
    && apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY . ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/ops-extension .

FROM scratch
COPY --from=build /out/ops-extension /ops-extension
ENTRYPOINT ["/ops-extension"]
