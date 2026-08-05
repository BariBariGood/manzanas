package state

import (
	"context"
	"fmt"
	"sort"
)

// statusBarFixture drives `simctl status_bar <udid> override ...` (or
// `clear`). Payload: {"clear": true} or a map of override names to values,
// e.g. {"time": "9:41", "batteryLevel": 100, "batteryState": "charged",
// "wifiBars": 3, "cellularBars": 4, "dataNetwork": "5g",
// "operatorName": "manzanasd"}. Keys map 1:1 to simctl status_bar flags.
// Requires a booted target.
type statusBarFixture struct{}

// statusBarOverrides is the allowlist of `simctl status_bar override`
// flags mapped to the expected JSON value kind; unknown payload keys are
// rejected rather than passed through as arbitrary command-line flags,
// and wrong-typed values fail as bad_request instead of surfacing as an
// opaque simctl error.
var statusBarOverrides = map[string]string{
	"time":               "string",
	"dataNetwork":        "string",
	"wifiMode":           "string",
	"wifiBars":           "number",
	"cellularMode":       "string",
	"cellularBars":       "number",
	"operatorName":       "string",
	"batteryState":       "string",
	"batteryLevel":       "number",
	"raiseToListenState": "string",
}

func checkOverrideType(key string, v any) error {
	want := statusBarOverrides[key]
	var ok bool
	switch want {
	case "string":
		_, ok = v.(string)
	case "number":
		switch v.(type) {
		case float64, int, int64:
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("%w: statusbar override %q must be a %s", ErrBadFixture, key, want)
	}
	return nil
}

func (statusBarFixture) Name() string { return "statusbar" }

func (statusBarFixture) Apply(ctx context.Context, run Runner, udid string, payload map[string]any) error {
	if clear, _ := payload["clear"].(bool); clear {
		_, err := run.Simctl(ctx, "status_bar", udid, "clear")
		return err
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		if k == "clear" {
			continue
		}
		if _, known := statusBarOverrides[k]; !known {
			return fmt.Errorf("%w: unknown statusbar override %q", ErrBadFixture, k)
		}
		if err := checkOverrideType(k, payload[k]); err != nil {
			return err
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := []string{"status_bar", udid, "override"}
	for _, k := range keys {
		v := fmt.Sprintf("%v", normalizeNumber(payload[k]))
		if err := flagSafe(v); err != nil {
			return fmt.Errorf("%w: %q: %v", ErrBadFixture, k, err)
		}
		args = append(args, "--"+k, v)
	}
	if len(args) == 3 {
		return fmt.Errorf("%w: statusbar payload must set at least one override or clear", ErrBadFixture)
	}
	_, err := run.Simctl(ctx, args...)
	return err
}

// normalizeNumber renders whole JSON numbers (float64) without a decimal
// point so e.g. batteryLevel 100 becomes "100", not "100.000000".
func normalizeNumber(v any) any {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return int64(f)
	}
	return v
}
