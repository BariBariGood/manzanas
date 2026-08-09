package actions

import (
	"reflect"
	"testing"
)

func TestIsScrollIndicator(t *testing.T) {
	bar := anode("Slider", "", 389, 100, 3, 300, true) // thin vertical bar
	if !isScrollIndicator(bar) {
		t.Fatalf("thin unlabeled Slider bar should be a scroll indicator")
	}
	horizontal := anode("ScrollBar", "", 20, 840, 300, 4, false)
	if !isScrollIndicator(horizontal) {
		t.Fatalf("thin horizontal ScrollBar should be a scroll indicator")
	}
	systemLabeled := anode("Slider", "Vertical scroll bar, 2 pages", 369, 116, 30, 672, true)
	if !isScrollIndicator(systemLabeled) {
		t.Fatalf("UIKit-labeled scroll bar should be a scroll indicator (seen live in stock Settings)")
	}
	labeled := anode("Slider", "Volume", 20, 100, 300, 6, true)
	if isScrollIndicator(labeled) {
		t.Fatalf("labeled Slider (a real control) must not be chrome")
	}
	identified := anode("Slider", "", 20, 100, 300, 6, true)
	identified.Identifier = "brightness_slider"
	if isScrollIndicator(identified) {
		t.Fatalf("identified Slider must not be chrome")
	}
	square := anode("Slider", "", 20, 100, 30, 30, true)
	if isScrollIndicator(square) {
		t.Fatalf("square Slider is not elongated; must not be chrome")
	}
	thick := anode("Slider", "", 20, 100, 300, 30, true)
	if isScrollIndicator(thick) {
		t.Fatalf("thick Slider must not be chrome")
	}
	button := anode("Button", "", 389, 100, 3, 300, true)
	if isScrollIndicator(button) {
		t.Fatalf("non-Slider roles are never scroll indicators")
	}
}

func TestIsSystemChrome(t *testing.T) {
	key := anode("Key", "q", 10, 700, 32, 42, true)
	keyboard := anode("Keyboard", "", 0, 650, 393, 200, false, key)
	statusItem := anode("StaticText", "9:41", 20, 10, 40, 20, false)
	statusBar := anode("Other", "", 0, 0, 393, 54, false, statusItem)
	statusBar.Identifier = "UIStatusBar-1"
	appButton := anode("Button", "Save", 20, 100, 44, 44, true)
	nodes := []*Node{keyboard, statusBar, appButton}
	parents := map[*Node]*Node{}
	var link func(ns []*Node, p *Node)
	link = func(ns []*Node, p *Node) {
		for _, n := range ns {
			parents[n] = p
			link(n.Children, n)
		}
	}
	link(nodes, nil)

	for _, n := range []*Node{key, keyboard, statusItem, statusBar} {
		if !isSystemChrome(n, parents) {
			t.Fatalf("%s %q should be system chrome", n.Role, n.Label)
		}
	}
	if isSystemChrome(appButton, parents) {
		t.Fatalf("app content must not be classified as chrome")
	}
}

// settingsFixture models a stock Settings screen from the MobAI
// comparison report: full-width tappable rows (Cells, and the Button
// rows iOS 26 Settings uses) containing small (~28pt) label Buttons and
// chevrons, plus a scroll indicator — and one genuinely tiny standalone
// app button that must stay flagged.
func settingsFixture() []*Node {
	row := func(role string, y float64, label string) *Node {
		inner := anode("Button", label, 60, y+9, 200, 28, true)
		chevron := anode("Button", "chevron", 370, y+16, 10, 14, true)
		return anode(role, label, 0, y, 393, 46, true, inner, chevron)
	}
	return []*Node{
		row("Cell", 100, "General"),
		row("Button", 146, "Accessibility"), // iOS 26 Settings surfaces rows as Buttons
		row("Cell", 192, "Privacy & Security"),
		anode("Slider", "Vertical scroll bar, 2 pages", 369, 100, 30, 672, true), // scroll indicator (as seen live)
		anode("Button", "Tiny", 20, 600, 28, 28, true),                           // real finding
	}
}

func TestAuditSuppressesSettingsNoise(t *testing.T) {
	rep := runAudit(settingsFixture(), auditViewport, auditOnly("touch_target"))
	got := findingsFor(rep, "touch_target")
	if len(got) != 1 || got[0].Element.Label != "Tiny" {
		t.Fatalf("touch_target findings = %+v, want only Tiny", got)
	}
	if rep.covered != 6 {
		t.Fatalf("covered = %d, want 6 row-covered controls", rep.covered)
	}
	if rep.suppressed == 0 {
		t.Fatalf("scroll indicator should be counted in suppressed")
	}
}

func TestAuditIncludeFlagsRestoreFindings(t *testing.T) {
	cfg := auditOnly("touch_target")
	cfg.includeChrome = true
	cfg.includeCovered = true
	rep := runAudit(settingsFixture(), auditViewport, cfg)
	got := findingsFor(rep, "touch_target")
	// 3 inner row buttons + 3 chevrons + scroll indicator + Tiny.
	if len(got) != 8 {
		t.Fatalf("touch_target findings = %d (%+v), want 8 with suppression off", len(got), got)
	}
	if rep.covered != 0 {
		t.Fatalf("covered = %d, want 0 with include_covered_controls", rep.covered)
	}
}

func TestAuditChromeSuppressionDeterministic(t *testing.T) {
	first := runAudit(settingsFixture(), auditViewport, auditOnly("touch_target"))
	for i := 0; i < 5; i++ {
		rep := runAudit(settingsFixture(), auditViewport, auditOnly("touch_target"))
		if !reflect.DeepEqual(rep.findings, first.findings) ||
			rep.covered != first.covered || rep.suppressed != first.suppressed {
			t.Fatalf("run %d differed: %+v vs %+v", i, rep, first)
		}
	}
}
