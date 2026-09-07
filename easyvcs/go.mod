module easyvcs

go 1.26.5

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/cpuid/v2 v2.0.9 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/sabhiram/go-gitignore v0.0.0-20210923224102-525f6e181f06
	golang.org/x/sys v0.47.0 // indirect
	lukechampine.com/blake3 v1.4.1
	modernc.org/sqlite v1.58.0
)

require (
	connectrpc.com/connect v1.20.0
	github.com/easylab-platform/easylab-proto v0.1.0
	github.com/abcp-sdk/agent-proto v0.1.0
)

require (
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.75.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

replace github.com/easylab-platform/easylab-proto => ../../proto/gen/go

