module github.com/projectbeskar/virtrigaud/sdk

go 1.24.0

require (
	github.com/projectbeskar/virtrigaud v0.1.0
	github.com/projectbeskar/virtrigaud/proto v0.1.0
	google.golang.org/grpc v1.79.3
)

require (
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)

// For local development, replace with local modules
replace github.com/projectbeskar/virtrigaud/proto => ../proto

replace github.com/projectbeskar/virtrigaud => ../
