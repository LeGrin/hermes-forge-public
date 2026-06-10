package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// startHTTPServer starts a StreamableHTTP MCP server on the given address
// (e.g. ":8095" or "127.0.0.1:0" for a random port in tests).
//
// It returns the actual bound address and a shutdown function.
// The caller is responsible for calling shutdown when done.
//
// Transport: MCP Streamable HTTP (2025-03-26 spec), supported by mcp-go v0.43.2.
// Clients connect to POST /mcp for JSON-RPC requests.
// GET /mcp opens an SSE stream for server-initiated messages.
func startHTTPServer(s *server.MCPServer, addr string) (boundAddr string, shutdown func()) {
	// Use a real net.Listener so we can discover the OS-assigned port when
	// addr contains port 0 (used in tests).
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		panic(fmt.Sprintf("hermes-mcp: listen %s: %v", addr, err))
	}
	boundAddr = ln.Addr().String()

	httpSrv := &http.Server{
		Addr:         boundAddr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE streams must not time out on write
		IdleTimeout:  120 * time.Second,
	}

	mcpHTTP := server.NewStreamableHTTPServer(s,
		server.WithStreamableHTTPServer(httpSrv),
		// Stateless mode: no server-side session state between requests.
		// Each JSON-RPC call is self-contained, matching the stdio model.
		// Switch to WithStateful(true) if you need server-push notifications.
		server.WithStateLess(true),
	)

	httpSrv.Handler = mcpHTTP

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.Serve(ln)
	}()

	shutdown = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		<-errCh
	}
	return boundAddr, shutdown
}
