// Command prometheus is the MCP server of Zero Root for the Prometheus HTTP
// API. It is the health signal source of the fix loop. It only reads.
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("prometheus", Version, build)) }
