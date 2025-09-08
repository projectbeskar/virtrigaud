module github.com/projectbeskar/virtrigaud/sdk

go 1.23.0

require (
	github.com/projectbeskar/virtrigaud v0.1.0
	github.com/projectbeskar/virtrigaud/proto v0.1.0
	google.golang.org/grpc v1.75.0
)

require (
	golang.org/x/net v0.42.0 // indirect
	golang.org/x/sys v0.34.0 // indirect
	golang.org/x/text v0.28.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250707201910-8d1bb00bc6a7 // indirect
	google.golang.org/protobuf v1.36.6 // indirect
)

// For local development, replace with local modules
replace github.com/projectbeskar/virtrigaud/proto => ../proto

replace github.com/projectbeskar/virtrigaud => ../
