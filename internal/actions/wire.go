package actions

import (
	"errors"

	"github.com/BariBariGood/manzanas/proto"
)

// WireError maps a backend error onto the protocol error shape so the
// server can translate it without knowing the backend's internals.
func WireError(err error) *proto.Error {
	var ae *Error
	if errors.As(err, &ae) {
		return &proto.Error{Code: ae.Code, Message: ae.Message}
	}
	return &proto.Error{Code: proto.ErrInternal, Message: err.Error()}
}
