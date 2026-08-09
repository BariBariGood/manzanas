package mcp

import (
	"errors"

	"github.com/BariBariGood/manzanas/internal/client"
	"github.com/BariBariGood/manzanas/proto"
)

// withHint renders a tool error with a recovery hint appended, so an LLM
// agent always knows what to do next without consulting the docs.
func withHint(err error) string {
	msg := err.Error()
	if hint := hintFor(err); hint != "" {
		return msg + ". " + hint
	}
	return msg
}

// hintFor maps well-known failure classes to a next-step instruction.
func hintFor(err error) string {
	var ce *client.ConnError
	if errors.As(err, &ce) {
		return "The manzanasd daemon is unreachable. Check that it is running (curl " +
			ce.Addr + "/v0/healthz from the machine running this MCP server), and that " +
			"the server points at the right address (MANZANASD_ADDR env var or the " +
			"--daemon flag of `manzanas mcp`)"
	}
	var ae *client.APIError
	if !errors.As(err, &ae) {
		return ""
	}
	switch ae.Code {
	case proto.ErrLeaseExpired:
		return "The lease is no longer active. Call lease_acquire to claim a target " +
			"again; on long sessions call lease_renew before the TTL runs out " +
			"(there is a short grace window after nominal expiry in which a renew " +
			"still rescues the lease, but do not rely on it)"
	case proto.ErrNotFound:
		return "If this refers to a lease_id, the lease is unknown to the daemon " +
			"(expired or released). Call lease_acquire to get a fresh lease_id"
	case proto.ErrNoMatch:
		return "No target matches the requested labels/udid. Call targets to list " +
			"available targets and their labels, then retry with labels that exist"
	case proto.ErrTargetNotBooted:
		return "The target is shut down. Call lease_release then lease_acquire again " +
			"(lease_acquire boots the leased target by default before returning; " +
			"do not pass boot=false), or pick a Booted target from targets"
	case proto.ErrTargetBusy:
		return "The target is busy in another lifecycle operation. Wait a few seconds " +
			"and retry"
	case proto.ErrOverloaded:
		return "The daemon hit a concurrency limit. Back off for a few seconds and retry"
	case proto.ErrTimeout:
		return "The action's wait budget was exhausted. The UI may just be slow: call " +
			"ui_tree to inspect the current screen, then retry"
	case proto.ErrOffViewport:
		return "The coordinates fall outside the device screen. Call ui_tree and use " +
			"points inside the element frames it returns, or use tap_element / " +
			"scroll_to_element instead of raw coordinates"
	case proto.ErrAmbiguousMatch:
		return "The predicate matched several elements (they are listed in the error). " +
			"Tighten it (add type, accessibility_id, bounds_hint, or near) or pick one " +
			"deterministically with index"
	case proto.ErrFocusRequired:
		return "No text field has keyboard focus. Use type_into_element to focus and " +
			"type in one step, or tap the target field first (find it with ui_tree) " +
			"and retry"
	case proto.ErrUnavailable:
		return "A required tool is missing on the daemon host; this needs an operator " +
			"to fix the host. Report the error and try a different target"
	}
	return ""
}
