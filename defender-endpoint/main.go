// Command defender-endpoint is the MCP server of Zero Root for response actions on Microsoft Defender for Endpoint devices.
// The vendor server of Microsoft (Sentinel MCP triage tools) has no tool to isolate a device. This server changes a user (ADR-0180).
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("defender-endpoint", Version, build)) }
