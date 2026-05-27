package main

import (
	"log"

	"github.com/mohamamd-y-abbass/mearch/internal/mcp"
)

func main() {
	server := mcp.New()

	if err := server.Run(); err != nil {
		log.Fatalf("mearch MCP server failed: %v", err)
	}
}
