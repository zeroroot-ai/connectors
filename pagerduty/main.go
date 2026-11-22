// Command pagerduty is the MCP server of Zero Root for paging with PagerDuty:
// alerts on the Events API v2 and a read of incidents on the REST API v2.
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("pagerduty", Version, build)) }
