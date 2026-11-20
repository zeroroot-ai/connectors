// Command crowdstrike is the MCP server of Zero Root for response actions on CrowdStrike Falcon hosts.
// The vendor server of CrowdStrike (falcon-mcp) has no tool to contain a host. This server changes a user (ADR-0180).
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("crowdstrike", Version, build)) }
