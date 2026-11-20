package kit

import (
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"
)

// ErrInvalidInput marks an input that the server refuses. The tool result
// tells the model what field was wrong. It does not repeat the value.
var ErrInvalidInput = errors.New("invalid input")

// Common formats. A pattern is anchored and has no whitespace and no shell
// metacharacter. The server never builds a shell command from an input, and
// the pattern is the second defence against a path or a header injection.
var (
	// UUID matches a UUID with hyphens.
	UUID = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
	// Hex32 matches 32 hexadecimal characters.
	Hex32 = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	// Hex40 matches 40 hexadecimal characters.
	Hex40 = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	// Alnum matches letters and digits.
	Alnum = regexp.MustCompile(`^[A-Za-z0-9]+$`)
)

// CheckString refuses an empty value, a value longer than limit bytes, a value
// that is not valid UTF-8, a value that holds a control character, and a value
// that does not match re (when re is not nil).
func CheckString(field, v string, limit int, re *regexp.Regexp) error {
	switch {
	case v == "":
		return fmt.Errorf("%w: %s is required", ErrInvalidInput, field)
	case len(v) > limit:
		return fmt.Errorf("%w: %s is longer than %d bytes", ErrInvalidInput, field, limit)
	case !utf8.ValidString(v):
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidInput, field)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %s holds a control character", ErrInvalidInput, field)
		}
	}
	if re != nil && !re.MatchString(v) {
		return fmt.Errorf("%w: %s has the wrong format", ErrInvalidInput, field)
	}
	return nil
}

// CheckRange refuses an integer outside [lo, hi].
func CheckRange(field string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("%w: %s must be from %d to %d", ErrInvalidInput, field, lo, hi)
	}
	return nil
}
