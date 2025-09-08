module github.com/projectbeskar/virtrigaud/sdk

go 1.23

require (
	github.com/projectbeskar/virtrigaud v0.1.0
	github.com/projectbeskar/virtrigaud/proto v0.1.0
	google.golang.org/grpc v1.67.1
	google.golang.org/protobuf v1.35.1
)

require (
	golang.org/x/net v0.28.0 // indirect
	golang.org/x/sys v0.24.0 // indirect
	golang.org/x/text v0.17.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240814211410-ddb44dafa142 // indirect
)

// For local development, replace with local modules
replace github.com/projectbeskar/virtrigaud/proto => ../proto
replace github.com/projectbeskar/virtrigaud => ../
