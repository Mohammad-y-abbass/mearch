package main

import (
	"fmt"
	"log"
	"os"

	"github.com/mohamamd-y-abbass/mearch/internal/mcp"
)

func main() {
	fmt.Fprintln(os.Stderr, "MAIN ENTRY HIT")
	server := mcp.New()

	if err := server.Run(); err != nil {
		log.Fatalf("mearch MCP server failed: %v", err)
	}
}
