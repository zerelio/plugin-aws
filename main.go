// Package main is the entry point for the AWS discovery plugin.
// It builds into a standalone binary that communicates with the Knowledge Tree
// orchestrator over gRPC using the hashicorp/go-plugin framework.
//
// Build:
//
//	go build -o kt-plugin-aws .
//
// Usage:
//
//	KT_PLUGIN_LOG_LEVEL=debug ./kt-plugin-aws
package main

import (
	sdk "github.com/zerelio/plugin-sdk"
)

func main() {
	sdk.Serve(&AWSPlugin{})
}
