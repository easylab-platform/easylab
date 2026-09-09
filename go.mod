module github.com/easylab-platform/ext-ops

go 1.26.3

require (
	connectrpc.com/connect v1.20.0
	github.com/abcp-sdk/abc-protocol-go v1.1.0
	github.com/easylab-platform/easylab-sdk-go v0.8.1
	github.com/go-chi/chi/v5 v5.3.2
	github.com/google/uuid v1.6.0
)

require (
	github.com/nats-io/nats.go v1.53.1 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

require (
	github.com/easylab-platform/easylab-proto v0.5.2
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/abcp-sdk/agent-proto v0.5.1 // indirect
	github.com/klauspost/compress v1.19.1 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/easylab-platform/easylab-sdk-go => ../easylab-sdk-go

replace github.com/easylab-platform/easylab-proto => ../easylab-proto
