package actions

import "fmt"

// Error is a protocol-mappable action error. Code is one of the proto
// well-known error codes (bad_request, unavailable, internal, ...).
type Error struct {
	Code    string
	Message string
	// escalation, when non-nil, is the resolver ladder's compact
	// observation bundle (rung trail + last text boxes); escalationPNG
	// is the best-effort screen capture that goes with it. Extracted
	// via EscalationEvidence, journaled by the server.
	escalation    map[string]any
	escalationPNG []byte
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func badRequest(format string, a ...any) *Error {
	return &Error{Code: "bad_request", Message: fmt.Sprintf(format, a...)}
}

func unavailable(format string, a ...any) *Error {
	return &Error{Code: "unavailable", Message: fmt.Sprintf(format, a...)}
}

func notImplemented(format string, a ...any) *Error {
	return &Error{Code: "not_implemented", Message: fmt.Sprintf(format, a...)}
}

func forbidden(format string, a ...any) *Error {
	return &Error{Code: "forbidden", Message: fmt.Sprintf(format, a...)}
}

func internal(format string, a ...any) *Error {
	return &Error{Code: "internal", Message: fmt.Sprintf(format, a...)}
}

func targetNotBooted(udid string) *Error {
	target := "target"
	if udid != "" {
		target = "target " + udid
	}
	return &Error{Code: "target_not_booted", Message: target + " is not booted; boot it before dispatching actions"}
}

func timeoutErr(format string, a ...any) *Error {
	return &Error{Code: "timeout", Message: fmt.Sprintf(format, a...)}
}

func offViewport(format string, a ...any) *Error {
	return &Error{Code: "off_viewport", Message: fmt.Sprintf(format, a...)}
}

func focusRequired(format string, a ...any) *Error {
	return &Error{Code: "focus_required", Message: fmt.Sprintf(format, a...)}
}
