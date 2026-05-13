package main

import (
	"github.com/DouDOU-start/airgate-kiro/backend/internal/gateway"
	sdkgrpc "github.com/DouDOU-start/airgate-sdk/runtimego/grpc"
)

func main() {
	sdkgrpc.Serve(&gateway.KiroGateway{})
}
