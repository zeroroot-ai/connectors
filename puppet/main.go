// Command puppet is the MCP server of Zero Root for patch jobs on Puppet Enterprise nodes.
// The MCP server of Puppet Enterprise only helps with code and has no patch tool.
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("puppet", Version, build)) }
