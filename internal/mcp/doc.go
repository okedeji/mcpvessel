// Package mcp wraps the official Go MCP SDK with the narrow surface
// agentcage needs: connect over stdio or HTTP, list tools, call one, read
// the text result. It is the only package in the repo allowed to import
// github.com/modelcontextprotocol/go-sdk/mcp; SDK churn stays here.
package mcp
