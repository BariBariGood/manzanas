package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BariBariGood/manzanas/proto"
)

// Tool is one MCP tool: schema plus handler. Implementations live in the
// tools_*.go files; allTools is the registry.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Call        func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error)
}

// allTools indexes every tool the facade exposes.
func allTools() []Tool {
	return []Tool{
		toolLeaseAcquire(),
		toolLeaseRelease(),
		toolLeaseRenew(),
		toolTargets(),
		toolObserve(),
		toolUITree(),
		toolTapElement(),
		toolTypeIntoElement(),
		toolScrollToElement(),
		toolWaitForElement(),
		toolWaitTreeStable(),
		toolTap(),
		toolSwipe(),
		toolType(),
		toolButton(),
		toolScreenshot(),
		toolRecordStart(),
		toolRecordStop(),
		toolApp(),
		toolState(),
		toolJournalExport(),
	}
}

// schema builds an MCP inputSchema from property name -> {type, description}
// pairs plus the required list.
func schema(props map[string]map[string]any, required ...string) map[string]any {
	p := make(map[string]any, len(props))
	for k, v := range props {
		p[k] = v
	}
	s := map[string]any{"type": "object", "properties": p}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

// reqStr returns the named argument as a non-empty string or an error.
func reqStr(args map[string]any, key string) (string, error) {
	v, ok := args[key].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("missing or invalid %q argument (non-empty string required)", key)
	}
	return v, nil
}

// reqNum returns the named argument as a JSON number or an error.
func reqNum(args map[string]any, key string) (float64, error) {
	v, ok := args[key].(float64)
	if !ok {
		return 0, fmt.Errorf("missing or invalid %q argument (number required)", key)
	}
	return v, nil
}

// actionContent converts an ActionResult into MCP content, turning
// OK==false into an error so the agent sees isError instead of a
// success-looking payload.
func actionContent(res proto.ActionResult) ([]map[string]any, error) {
	if err := actionErr(res); err != nil {
		return nil, err
	}
	return jsonContent(res)
}

// actionErr returns an error describing a failed action, or nil if OK.
func actionErr(res proto.ActionResult) error {
	if res.OK {
		return nil
	}
	if len(res.Result) > 0 {
		b, _ := json.Marshal(res.Result)
		return fmt.Errorf("action failed: %s", b)
	}
	return fmt.Errorf("action failed")
}

func num(args map[string]any, key string) float64 {
	v, _ := args[key].(float64)
	return v
}
