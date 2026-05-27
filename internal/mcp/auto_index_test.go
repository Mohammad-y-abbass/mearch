package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRootPathFromURI(t *testing.T) {
	t.Parallel()
	root := mustAbs(t, ".")
	uri := "file:///" + filepath.ToSlash(root)
	got, err := rootPathFromURI(uri)
	if err != nil {
		t.Fatalf("rootPathFromURI: %v", err)
	}
	want := filepath.Clean(root)
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAutoIndexOnInitialized(t *testing.T) {
	ctx := context.Background()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()

	s := New()
	mcpServer := sdk.NewServer(&sdk.Implementation{Name: "mearch", Version: "test"}, &sdk.ServerOptions{
		InitializedHandler: func(ctx context.Context, req *sdk.InitializedRequest) {
			s.autoIndexOnConnect(ctx, req.Session)
		},
	})
	s.registerIndexProject(mcpServer)
	s.registerGraphStats(mcpServer)

	_, err := mcpServer.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "test"}, nil)
	client.AddRoots(&sdk.Root{URI: "file:///" + filepath.ToSlash(mustAbs(t, "."))})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		ready := s.g != nil
		s.mu.RUnlock()
		if ready {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.g == nil {
		t.Fatal("graph was not built after client initialized")
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
