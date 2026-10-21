// Package mcptest provides mock MCP servers for tests (stdio via test-binary
// re-exec, Streamable HTTP and legacy SSE via httptest).
package mcptest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// EnvMode selects stdio mock behavior when the test binary is re-executed:
//
//	ok    : serve tools listed in MOCK_TOOLS (comma separated; default "alpha,beta"),
//	        each described by MOCK_DESC (default "mock tool")
//	hang  : never answer
//	exit  : write MOCK_STDERR to stderr and exit 3 immediately
//	flood : write 6 MiB of a never-terminated JSON string without a newline, then hang
//	noise : write non-JSON to stdout, then hang
const EnvMode = "FORGE_MCP_MOCK"

// MaybeRunStdio must be called first in TestMain: when the process was started
// as a mock stdio server it serves and exits, otherwise it returns.
func MaybeRunStdio() {
	mode := os.Getenv(EnvMode)
	if mode == "" {
		return
	}
	switch mode {
	case "hang":
		time.Sleep(time.Hour)
	case "exit":
		fmt.Fprintln(os.Stderr, os.Getenv("MOCK_STDERR"))
		os.Exit(3)
	case "flood":
		os.Stdout.WriteString(`{"jsonrpc":"2.0","id":1,"result":"`)
		chunk := []byte(strings.Repeat("a", 64<<10))
		for i := 0; i < 96; i++ {
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(1)
			}
		}
		time.Sleep(time.Hour)
	case "noise":
		fmt.Println("this is not json-rpc")
		time.Sleep(time.Hour)
	default:
		tools := os.Getenv("MOCK_TOOLS")
		if tools == "" {
			tools = "alpha,beta"
		}
		desc := os.Getenv("MOCK_DESC")
		if desc == "" {
			desc = "mock tool"
		}
		if err := NewServer(strings.Split(tools, ","), desc).Run(context.Background(), &sdk.StdioTransport{}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(0)
}

// NewServer builds a mock MCP server exposing the given tools.
func NewServer(tools []string, desc string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "mock-mcp", Version: "1.2.3"}, nil)
	for _, n := range tools {
		s.AddTool(&sdk.Tool{
			Name:        n,
			Description: desc,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
		}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
		})
	}
	return s
}

// Auth wraps a handler: requests without the exact Authorization header get 401.
func Auth(want string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// HTTP starts a Streamable HTTP mock.
func HTTP(tools []string, desc string) *httptest.Server {
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return NewServer(tools, desc) }, nil)
	return httptest.NewServer(h)
}

// SSE starts a legacy SSE mock.
func SSE(tools []string, desc string) *httptest.Server {
	h := sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return NewServer(tools, desc) }, nil)
	return httptest.NewServer(h)
}

// AuthHTTP is HTTP behind an exact Authorization header check.
func AuthHTTP(tools []string, desc, want string) *httptest.Server {
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return NewServer(tools, desc) }, nil)
	return httptest.NewServer(Auth(want, h))
}
