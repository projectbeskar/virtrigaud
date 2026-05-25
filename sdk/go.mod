module github.com/projectbeskar/virtrigaud/sdk

go 1.26.0

require (
	github.com/projectbeskar/virtrigaud v0.1.0
	github.com/projectbeskar/virtrigaud/proto v0.1.0
	google.golang.org/grpc v1.80.0
)

require (
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.35.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// For local development, replace with local modules
replace github.com/projectbeskar/virtrigaud/proto => ../proto

replace github.com/projectbeskar/virtrigaud => ../
