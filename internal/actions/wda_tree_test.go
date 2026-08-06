package actions

import (
	"testing"
)

const sampleWDAXML = `<?xml version="1.0" encoding="UTF-8"?>
<XCUIElementTypeApplication type="XCUIElementTypeApplication" name="Demo" label="Demo" enabled="true" visible="true" x="0" y="0" width="393" height="852">
  <XCUIElementTypeWindow type="XCUIElementTypeWindow" enabled="true" visible="true" x="0" y="0" width="393" height="852">
    <XCUIElementTypeOther type="XCUIElementTypeOther" enabled="true" visible="true" x="0" y="0" width="393" height="852">
      <XCUIElementTypeButton type="XCUIElementTypeButton" name="login-button" label="Log In" enabled="true" visible="true" x="20" y="700" width="353" height="44"/>
      <XCUIElementTypeTextField type="XCUIElementTypeTextField" name="email" label="Email" value="a@b.c" enabled="true" visible="true" x="20" y="200" width="353" height="44"/>
      <XCUIElementTypeStaticText type="XCUIElementTypeStaticText" label="Welcome" enabled="true" visible="true" x="20" y="100" width="353" height="30"/>
      <XCUIElementTypeButton type="XCUIElementTypeButton" name="disabled-btn" label="Nope" enabled="false" visible="true" x="20" y="760" width="353" height="44"/>
    </XCUIElementTypeOther>
  </XCUIElementTypeWindow>
</XCUIElementTypeApplication>`

func findByLabel(nodes []*Node, label string) *Node {
	for _, n := range nodes {
		if n.Label == label {
			return n
		}
		if hit := findByLabel(n.Children, label); hit != nil {
			return hit
		}
	}
	return nil
}

func TestWDAViewport(t *testing.T) {
	vp := wdaViewport(sampleWDAXML)
	if vp == nil || vp.W != 393 || vp.H != 852 {
		t.Fatalf("wdaViewport = %+v, want 393x852", vp)
	}
	if vp := wdaViewport("not xml"); vp != nil {
		t.Fatalf("wdaViewport on garbage = %+v, want nil", vp)
	}
}

func TestCompactWDATree(t *testing.T) {
	nodes, err := CompactWDATree(sampleWDAXML)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 {
		t.Fatal("empty tree")
	}

	btn := findByLabel(nodes, "Log In")
	if btn == nil {
		t.Fatal("Log In button not found")
	}
	if btn.Role != "Button" || !btn.Interactable {
		t.Fatalf("button role/interactable: %+v", btn)
	}
	if btn.Identifier != "login-button" {
		t.Fatalf("identifier = %q, want login-button", btn.Identifier)
	}
	if btn.Frame == nil || btn.Frame.X != 20 || btn.Frame.Y != 700 || btn.Frame.W != 353 || btn.Frame.H != 44 {
		t.Fatalf("frame = %+v", btn.Frame)
	}

	field := findByLabel(nodes, "Email")
	if field == nil || field.Role != "TextField" || field.Value != "a@b.c" {
		t.Fatalf("email field: %+v", field)
	}

	disabled := findByLabel(nodes, "Nope")
	if disabled == nil || !disabled.Disabled {
		t.Fatalf("disabled button: %+v", disabled)
	}

	if h := TreeHash(nodes); h == "" {
		t.Fatal("empty hash")
	}
}

func TestCompactWDATreeBadXML(t *testing.T) {
	if _, err := CompactWDATree("not xml at all <"); err == nil {
		t.Fatal("want error for invalid XML")
	}
}
