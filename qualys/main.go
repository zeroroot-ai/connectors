// Command qualys is the MCP server of Zero Root for Qualys VMDR. It only reads.
// Qualys publishes no MCP server for a customer. This server changes a user (ADR-0180).
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("qualys", Version, build)) }
