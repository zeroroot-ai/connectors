// Command servicenow is the MCP server of Zero Root for incidents in
// ServiceNow, on the Table API. The tool names follow internal/ticket.
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("servicenow", Version, build)) }
