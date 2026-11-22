// Command rapid7-insightidr is the MCP server of Zero Root for Rapid7
// InsightIDR. It reads investigations and their alerts.
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("rapid7-insightidr", Version, build)) }
