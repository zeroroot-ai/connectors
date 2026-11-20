// Command sentinelone is the MCP server of Zero Root for response actions on SentinelOne agents.
// The vendor server of SentinelOne (purple-mcp) is read only. This server changes a user (ADR-0180).
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("sentinelone", Version, build)) }
