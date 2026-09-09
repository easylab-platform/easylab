module github.com/easylab-platform/easylab

go 1.26.5

require (
	connectrpc.com/connect v1.20.0
	github.com/abcp-sdk/agent-proto v0.5.1
	github.com/docker/docker v28.5.2+incompatible
	github.com/docker/go-connections v0.8.1
	github.com/easylab-platform/artifact/cargo v0.2.0
	github.com/easylab-platform/artifact/composer v0.2.0
	github.com/easylab-platform/artifact/conan v0.2.0
	github.com/easylab-platform/artifact/core v0.3.0
	github.com/easylab-platform/artifact/generic v0.2.0
	github.com/easylab-platform/artifact/go v0.2.0
	github.com/easylab-platform/artifact/helm v0.2.0
	github.com/easylab-platform/artifact/hex v0.2.0
	github.com/easylab-platform/artifact/maven v0.2.0
	github.com/easylab-platform/artifact/npm v0.2.0
	github.com/easylab-platform/artifact/nuget v0.2.0
	github.com/easylab-platform/artifact/oci v0.2.0
	github.com/easylab-platform/artifact/pub v0.2.0
	github.com/easylab-platform/artifact/pypi v0.2.0
	github.com/easylab-platform/artifact/rubygems v0.2.0
	github.com/easylab-platform/artifact/swiftpm v0.2.0
	github.com/easylab-platform/artifact/system v0.2.0
	github.com/easylab-platform/easylab-proto v0.5.2
	github.com/easylab-platform/easyvcs v0.5.1
	github.com/easylab-platform/ext-ops v0.0.0-20260908104207-dc244cee69ef
	github.com/easylab-platform/ext-repo v0.0.0-20260908104219-ccb664286cc7
	github.com/glebarez/sqlite v1.11.0
	gorm.io/driver/postgres v1.6.2
	gorm.io/gorm v1.31.2
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/abcp-sdk/abc-protocol-go v1.1.0 // indirect
	github.com/abcp-sdk/agent-sdk-go v0.5.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/errdefs v1.0.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/easylab-platform/easylab-sdk-go v0.8.1 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/glebarez/go-sqlite v1.21.2 // indirect
	github.com/go-chi/chi/v5 v5.3.2 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-sql-driver/mysql v1.8.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/klauspost/compress v1.19.1 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/sys/atomicwriter v0.1.0 // indirect
	github.com/moby/term v0.5.2 // indirect
	github.com/morikuni/aec v1.1.0 // indirect
	github.com/nats-io/nats.go v1.53.1 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/sabhiram/go-gitignore v0.0.0-20210923224102-525f6e181f06 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	gorm.io/driver/mysql v1.6.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
	lukechampine.com/blake3 v1.4.1 // indirect
	modernc.org/libc v1.75.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.58.0 // indirect
)

replace github.com/easylab-platform/easyvcs => ../easyvcs

replace github.com/easylab-platform/easylab-proto => ../easylab-proto

replace github.com/easylab-platform/ext-repo => ../ext-repo

replace github.com/easylab-platform/easylab-sdk-go => ../easylab-sdk-go

replace github.com/easylab-platform/ext-ops => ../ext-ops
