// Command rapid7-insightvm is the MCP server of Zero Root for Rapid7
// InsightVM (Console API v3). It reads assets and their vulnerabilities.
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("rapid7-insightvm", Version, build)) }
