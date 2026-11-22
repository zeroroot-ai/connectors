// Command microsoft-intune is the MCP server of Zero Root for Windows patches
// through Microsoft Intune and the Windows Update deployment service of
// Microsoft Graph. No vendor server covers this.
package main

import (
	"os"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(kit.Run("microsoft-intune", Version, build)) }
