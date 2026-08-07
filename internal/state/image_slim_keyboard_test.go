package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProfile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSlimProfileFileKeyboardSafe(t *testing.T) {
	tests := []struct {
		name    string
		content string
		safe    bool
		wantErr bool
	}{
		{
			name:    "siri category excepted",
			content: `{"name":"qa","except":["store","siri"]}`,
			safe:    true,
		},
		{
			name:    "both daemons kept explicitly",
			content: `{"name":"qa","keep":["com.apple.assistantd","com.apple.corespeechd"]}`,
			safe:    true,
		},
		{
			name:    "only one daemon kept",
			content: `{"name":"qa","keep":["com.apple.assistantd"]}`,
			safe:    false,
		},
		{
			name:    "current fleet agent-qa shape",
			content: `{"name":"agent-qa","except":["store","web","photos"],"keep":["com.apple.akd","com.apple.devicecheckd"]}`,
			safe:    false,
		},
		{
			name:    "invalid JSON",
			content: `{`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			safe, err := slimProfileFileKeyboardSafe(writeProfile(t, tt.content))
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("slimProfileFileKeyboardSafe: %v", err)
			}
			if safe != tt.safe {
				t.Fatalf("safe = %v, want %v", safe, tt.safe)
			}
		})
	}
}

func TestSlimProfileKeyboardWarning(t *testing.T) {
	if msg, err := SlimProfileKeyboardWarning(""); err != nil || msg != "" {
		t.Fatalf("empty profile: msg=%q err=%v, want no warning", msg, err)
	}
	msg, err := SlimProfileKeyboardWarning("default")
	if err != nil || !strings.Contains(msg, "AFDictationConnection") {
		t.Fatalf("default profile: msg=%q err=%v, want AFDictationConnection warning", msg, err)
	}
}
