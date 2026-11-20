package kit

import (
	"io"
	"log/slog"
	"strings"
)

// secretKeyParts name the attribute keys whose value the logger replaces.
// This is the second defence. The first defence is the Secret type.
var secretKeyParts = []string{
	"authorization", "token", "secret", "password", "passwd", "credential",
	"cookie", "apikey", "api_key", "api-key", "private",
}

// NewLogger returns a JSON logger. It replaces the value of each attribute
// whose key looks like a credential.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			key := strings.ToLower(a.Key)
			for _, part := range secretKeyParts {
				if strings.Contains(key, part) {
					return slog.String(a.Key, redacted)
				}
			}
			return a
		},
	}))
}
