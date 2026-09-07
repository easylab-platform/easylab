module forgejo.develop.10.199.64.20.nip.io/abc-protocol/agent-sdk

go 1.26

replace forgejo.develop.10.199.64.20.nip.io/abc-protocol/agent-proto => ../../../proto/gen/agent-go

require (
	connectrpc.com/connect v1.20.0
	forgejo.develop.10.199.64.20.nip.io/abc-protocol/agent-proto v0.0.0-00010101000000-000000000000
)

require google.golang.org/protobuf v1.36.12 // indirect
