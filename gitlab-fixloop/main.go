// Command gitlab-fixloop is the MCP server of Zero Root for the fix loop on
// GitLab. It reads the state of a merge request and of its deployment, merges
// a merge request that is ready, and reverts a merge.
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("gitlab-fixloop", Version, build)) }
