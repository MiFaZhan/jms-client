//go:build tools

// Package tools pins build-time and not-yet-imported dependencies.
//
// The rewrite's remaining milestones were frozen in one contract commit, but
// several dependencies (the SSH client, the WebSocket client, the SFTP
// client, the MCP SDK) are only imported by packages that were still
// placeholders at that commit. Without this file `go mod tidy` sees no
// importer and silently drops them, leaving the contract claiming
// dependencies it does not actually carry — which is exactly what happened
// and blocked the transport lane.
//
// The blank imports below are the standard way to keep a chosen dependency in
// go.mod until real code imports it. This file is excluded from normal builds
// by the build tag; it is compiled only by `go mod tidy` and by tooling.
//
// When a package imports one of these directly, the corresponding line here
// becomes redundant and can be removed.
package tools

import (
	_ "github.com/gorilla/websocket"
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "github.com/pkg/sftp"
	_ "golang.org/x/crypto/ssh"
	_ "golang.org/x/term"
)
