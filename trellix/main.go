// Command trellix is the MCP server of Zero Root for Trellix EDR. It reads
// detections and host state. It has no isolate tool (see README).
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("trellix", Version, build)) }
