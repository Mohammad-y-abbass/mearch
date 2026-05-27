package mcp

import (
	"encoding/json"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// textResult wraps a string as an MCP text content result.
func textResult(text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		Content: []sdk.Content{
			&sdk.TextContent{Text: text},
		},
	}
}

// errorResult wraps an error as an MCP error result.
func errorResult(err error) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{
			&sdk.TextContent{Text: "error: " + err.Error()},
		},
	}
}

// jsonResult serializes a value as JSON and wraps it as an MCP text result.
// Used when structured data is more useful than formatted text.
func jsonResult(v any) *sdk.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errorResult(fmt.Errorf("failed to serialize result: %w", err))
	}
	return textResult(string(b))
}
