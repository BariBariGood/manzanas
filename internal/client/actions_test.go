package client

import (
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

func TestImageB64FormatDerivedKey(t *testing.T) {
	tests := []struct {
		name   string
		result map[string]any
		want   string
	}{
		{"jpeg via format", map[string]any{"format": "jpeg", "jpeg_base64": "J"}, "J"},
		{"png via format", map[string]any{"format": "png", "png_base64": "P"}, "P"},
		{"png without format", map[string]any{"png_base64": "P"}, "P"},
		{"jpeg without format", map[string]any{"jpeg_base64": "J"}, "J"},
		{"generic data", map[string]any{"data_base64": "D"}, "D"},
		{"no image", map[string]any{"bytes": 42}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ImageB64(proto.ActionResult{Result: tt.result}); got != tt.want {
				t.Fatalf("ImageB64 = %q, want %q", got, tt.want)
			}
		})
	}
}
