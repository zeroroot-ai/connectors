package kit

import "log/slog"

// redacted is the text that stands in for a secret in every output.
const redacted = "[redacted]"

// Secret holds a credential. Each text, log and JSON form of a Secret shows
// the word "[redacted]". Only Reveal returns the value, and only the code that
// sends the credential to the vendor calls it.
type Secret string

// String returns the redacted text.
func (Secret) String() string { return redacted }

// GoString returns the redacted text for the %#v verb.
func (Secret) GoString() string { return redacted }

// LogValue returns the redacted text to slog.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText returns the redacted text to the JSON and text encoders.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Reveal returns the credential value.
func (s Secret) Reveal() string { return string(s) }
