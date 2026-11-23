package kit

import (
	"errors"
	"fmt"
)

// GraphError wraps the error of a Microsoft Graph call for a tool result. It
// shows the Graph error code and message when the answer has them. The error
// body of Graph is {"error": {"code": "...", "message": "..."}}.
func GraphError(op string, err error) error {
	var ae *APIError
	if errors.As(err, &ae) {
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if DecodeErrorBody(ae, &body) == nil && body.Error.Code != "" {
			return Described(err, "%s: Graph answered HTTP %d, %s: %s", op, ae.Status, body.Error.Code, body.Error.Message)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}
