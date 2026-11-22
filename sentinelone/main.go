// Command sentinelone is the MCP server of Zero Root for response actions on SentinelOne agents.
// This server reads threats and agent state, and it changes the network state of an agent (ADR-0180).
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("sentinelone", Version, build)) }
