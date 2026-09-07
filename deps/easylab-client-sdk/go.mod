module forgejo.develop.10.199.64.20.nip.io/easylab/client-sdk

go 1.26

require (
	connectrpc.com/connect v1.20.0
	forgejo.develop.10.199.64.20.nip.io/easylab/easylab-proto v0.0.0
)

require google.golang.org/protobuf v1.36.12 // indirect

replace forgejo.develop.10.199.64.20.nip.io/easylab/easylab-proto => ../../../proto/gen/go
