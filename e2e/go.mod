module easyvcs-e2e

go 1.26

require forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go v0.0.0

require (
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/nats-io/nats.go v1.53.1 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)

replace forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go => ../deps/abc-sdk-go
