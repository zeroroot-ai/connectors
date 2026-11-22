// Command cortex-xdr is the MCP server of Zero Root for Palo Alto Networks
// Cortex XDR. It reads incidents, alerts and the state of endpoints.
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("cortex-xdr", Version, build)) }
