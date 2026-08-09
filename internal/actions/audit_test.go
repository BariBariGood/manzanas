package actions

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
)

var auditViewport = &Frame{X: 0, Y: 0, W: 393, H: 852}

func anode(role, label string, x, y, w, h float64, inter bool, kids ...*Node) *Node {
	return &Node{Role: role, Label: label, Frame: &Frame{X: x, Y: y, W: w, H: h},
		Interactable: inter, Children: kids}
}

func findingsFor(rep auditReport, check string) []auditFinding {
	var out []auditFinding
	for _, f := range rep.findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func auditOnly(check string) auditConfig {
	cfg := auditConfig{minTouch: auditMinTouchDefault, alignTol: auditAlignTolDefault,
		spacingTol: auditSpacingTolDefault}
	cfg.checks = map[string]bool{check: true}
	return cfg
}

func TestAuditTouchTarget(t *testing.T) {
	nodes := []*Node{
		anode("Button", "Tiny", 10, 100, 30, 30, true),
		anode("Button", "AtMinimum", 10, 200, 44, 44, true),
		anode("Button", "JitterOK", 10, 300, 43.8, 44, true), // within slack
		anode("Button", "Short", 10, 400, 100, 43, true),
		anode("Image", "Decor", 10, 500, 10, 10, false), // not interactive
	}
	rep := runAudit(nodes, auditViewport, auditOnly("touch_target"))
	got := findingsFor(rep, "touch_target")
	if len(got) != 2 {
		t.Fatalf("touch_target findings = %d (%+v), want 2", len(got), got)
	}
	if got[0].Element.Label != "Tiny" || got[1].Element.Label != "Short" {
		t.Fatalf("flagged %q and %q, want Tiny and Short", got[0].Element.Label, got[1].Element.Label)
	}
	if got[0].Measured["w_pt"] != 30.0 || got[0].Measured["min_pt"] != 44.0 {
		t.Fatalf("measured = %+v", got[0].Measured)
	}
	if !strings.Contains(got[0].Evidence, "30x30pt") {
		t.Fatalf("evidence = %q", got[0].Evidence)
	}
}

func TestAuditMissingLabels(t *testing.T) {
	withValue := anode("TextField", "", 10, 200, 200, 44, true)
	withValue.Value = "hello"
	withPlaceholder := anode("TextField", "", 10, 300, 200, 44, true)
	withPlaceholder.Placeholder = "Email"
	unlabeledID := anode("Button", "", 10, 400, 44, 44, true)
	unlabeledID.Identifier = "submit_btn"
	nodes := []*Node{
		anode("Button", "", 10, 100, 44, 44, true), // flagged
		withValue,
		withPlaceholder,
		unlabeledID, // flagged, id noted
		anode("StaticText", "", 10, 500, 80, 20, false), // not interactive
	}
	rep := runAudit(nodes, auditViewport, auditOnly("missing_labels"))
	got := findingsFor(rep, "missing_labels")
	if len(got) != 2 {
		t.Fatalf("missing_labels findings = %d (%+v), want 2", len(got), got)
	}
	if got[1].Measured["id"] != "submit_btn" || !strings.Contains(got[1].Evidence, "submit_btn") {
		t.Fatalf("id finding = %+v", got[1])
	}
}

func TestAuditClipping(t *testing.T) {
	scrollChild := anode("Cell", "Row", 0, 650, 393, 100, true) // extends past scroll parent: exempt
	nodes := []*Node{
		anode("Button", "OffRight", 350, 100, 80, 44, true), // 37pt past screen right
		anode("Other", "Card", 16, 200, 200, 100, false,
			anode("StaticText", "Overflowing", 16, 280, 200, 40, false)), // 20pt past parent bottom
		anode("ScrollView", "", 0, 400, 393, 300, false, scrollChild),
	}
	rep := runAudit(nodes, auditViewport, auditOnly("clipping"))
	got := findingsFor(rep, "clipping")
	if len(got) != 2 {
		t.Fatalf("clipping findings = %d (%+v), want 2", len(got), got)
	}
	if got[0].Measured["bounds"] != "screen" {
		t.Fatalf("first finding = %+v, want screen bounds", got[0])
	}
	over := got[0].Measured["overflow_pt"].(map[string]float64)
	if over["right"] != 37 {
		t.Fatalf("screen overflow = %+v, want right:37", over)
	}
	if got[1].Measured["bounds"] != "parent" || got[1].Element.Label != "Overflowing" {
		t.Fatalf("second finding = %+v, want parent-bounds Overflowing", got[1])
	}
}

func TestAuditClippingScrollContentPastScreenEdge(t *testing.T) {
	// Scroll content straddling the fold or a tall content container are
	// normal scrolling, not clipping — even against the screen bounds.
	nodes := []*Node{
		anode("ScrollView", "", 0, 0, 393, 852, false,
			anode("Other", "Content", 0, 0, 393, 2000, false,
				anode("Cell", "Fold", 0, 800, 393, 100, true))), // straddles screen bottom
	}
	rep := runAudit(nodes, auditViewport, auditOnly("clipping"))
	if got := findingsFor(rep, "clipping"); len(got) != 0 {
		t.Fatalf("clipping findings = %+v, want none for scroll content", got)
	}
}

func TestAuditScopedKeepsScrollAncestry(t *testing.T) {
	// A scoped audit re-roots at the matched node but must still see the
	// full tree's ancestry, or scroll content would be flagged as clipped.
	fold := anode("Cell", "Fold", 0, 800, 393, 100, true) // straddles screen bottom
	full := []*Node{anode("ScrollView", "", 0, 0, 393, 852, false,
		anode("Other", "Content", 0, 0, 393, 2000, false, fold))}
	rep := runAuditWithParents([]*Node{fold}, parentMap(full), auditViewport, auditOnly("clipping"))
	if got := findingsFor(rep, "clipping"); len(got) != 0 {
		t.Fatalf("clipping findings = %+v, want none for scoped scroll content", got)
	}
}

func TestAuditAlignmentNearMiss(t *testing.T) {
	nodes := []*Node{
		anode("Other", "", 0, 0, 393, 852, false,
			anode("Button", "A", 16, 100, 100, 44, true),
			anode("Button", "B", 18.5, 200, 100, 44, true), // left delta 2.5: near miss
			anode("Button", "C", 40, 300, 100, 44, true),   // delta >> tol: intentional
		),
	}
	rep := runAudit(nodes, auditViewport, auditOnly("alignment"))
	got := findingsFor(rep, "alignment")
	if len(got) != 2 { // left + right edges of A/B both off by 2.5
		t.Fatalf("alignment findings = %d (%+v), want 2", len(got), got)
	}
	if got[0].Measured["edge"] != "left" || got[0].Measured["delta_pt"] != 2.5 {
		t.Fatalf("measured = %+v", got[0].Measured)
	}
	if len(got[0].Related) != 1 || got[0].Related[0].Label != "B" {
		t.Fatalf("related = %+v", got[0].Related)
	}
}

func TestAuditAlignmentToleranceEdges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delta float64
		want  int
	}{
		{"sub-slack jitter ignored", 0.4, 0},
		{"just above slack flagged", 0.6, 2},
		{"at tolerance flagged", 4, 2},
		{"past tolerance intentional", 4.1, 0},
	} {
		nodes := []*Node{
			anode("Other", "", 0, 0, 393, 852, false,
				anode("Button", "A", 16, 100, 100, 44, true),
				anode("Button", "B", 16+tc.delta, 200, 100, 44, true),
			),
		}
		rep := runAudit(nodes, auditViewport, auditOnly("alignment"))
		if got := len(findingsFor(rep, "alignment")); got != tc.want {
			t.Errorf("%s (delta %v): findings = %d, want %d", tc.name, tc.delta, got, tc.want)
		}
	}
}

func TestAuditSpacing(t *testing.T) {
	nodes := []*Node{
		anode("Other", "", 0, 0, 393, 852, false,
			anode("StaticText", "One", 16, 100, 200, 20, false),
			anode("StaticText", "Two", 16, 132, 200, 20, false),   // gap 12
			anode("StaticText", "Three", 16, 164, 200, 20, false), // gap 12
			anode("StaticText", "Four", 16, 204, 200, 20, false),  // gap 20: deviates 8 > 4
		),
	}
	rep := runAudit(nodes, auditViewport, auditOnly("spacing"))
	got := findingsFor(rep, "spacing")
	if len(got) != 1 {
		t.Fatalf("spacing findings = %d (%+v), want 1", len(got), got)
	}
	f := got[0]
	if f.Element.Label != "Four" || f.Measured["gap_pt"] != 20.0 || f.Measured["median_gap_pt"] != 12.0 {
		t.Fatalf("finding = %+v", f)
	}
	if f.Measured["axis"] != "column" {
		t.Fatalf("axis = %v", f.Measured["axis"])
	}
}

func TestAuditSpacingNeedsThreeGaps(t *testing.T) {
	// Two gaps have no meaningful median (it is their average, so any
	// unequal pair deviates and flags both): 3-sibling groups never flag.
	nodes := []*Node{
		anode("Other", "", 0, 0, 393, 852, false,
			anode("StaticText", "Time", 16, 10, 60, 20, false),
			anode("StaticText", "Cellular", 269, 10, 30, 20, false), // gap 193
			anode("StaticText", "Battery", 330, 10, 40, 20, false),  // gap 31
		),
	}
	rep := runAudit(nodes, auditViewport, auditOnly("spacing"))
	if got := findingsFor(rep, "spacing"); len(got) != 0 {
		t.Fatalf("spacing findings on a 2-gap group = %+v, want none", got)
	}
}

func TestAuditSpacingToleranceEdge(t *testing.T) {
	// A gap deviating exactly the tolerance is not flagged; one point more is.
	build := func(lastGap float64) []*Node {
		return []*Node{
			anode("Other", "", 0, 0, 393, 852, false,
				anode("StaticText", "One", 16, 100, 200, 20, false),
				anode("StaticText", "Two", 16, 132, 200, 20, false),
				anode("StaticText", "Three", 16, 164, 200, 20, false),
				anode("StaticText", "Four", 16, 184+lastGap, 200, 20, false),
			),
		}
	}
	if rep := runAudit(build(16), auditViewport, auditOnly("spacing")); len(rep.findings) != 0 {
		t.Fatalf("deviation == tolerance should not be flagged: %+v", rep.findings)
	}
	if rep := runAudit(build(17), auditViewport, auditOnly("spacing")); len(rep.findings) != 1 {
		t.Fatalf("deviation > tolerance should be flagged: %+v", rep.findings)
	}
}

func TestAuditSafeArea(t *testing.T) {
	nodes := []*Node{
		anode("Button", "InNotch", 100, 20, 44, 44, true),    // top inset (59) overlap
		anode("Button", "AboveHome", 100, 830, 44, 44, true), // bottom inset (34) overlap
		anode("Button", "Safe", 100, 400, 44, 44, true),      // clear
		anode("StaticText", "Clock", 20, 10, 60, 20, false),  // not interactive: skipped
		anode("Other", "Background", 0, 0, 393, 852, true),   // full-bleed: exempt
	}
	rep := runAudit(nodes, auditViewport, auditOnly("safe_area"))
	got := findingsFor(rep, "safe_area")
	if len(got) != 2 {
		t.Fatalf("safe_area findings = %d (%+v), want 2", len(got), got)
	}
	if got[0].Element.Label != "InNotch" || got[0].Measured["edge"] != "top" {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].Element.Label != "AboveHome" || got[1].Measured["edge"] != "bottom" {
		t.Fatalf("second = %+v", got[1])
	}
}

func TestAuditSafeAreaExplicitInsets(t *testing.T) {
	cfg := auditOnly("safe_area")
	cfg.insets = &auditInsets{Top: 100}
	nodes := []*Node{
		anode("Button", "High", 100, 80, 44, 44, true),
		anode("Button", "Low", 100, 830, 44, 44, true), // no bottom inset configured
	}
	rep := runAudit(nodes, auditViewport, cfg)
	got := findingsFor(rep, "safe_area")
	if len(got) != 1 || got[0].Element.Label != "High" {
		t.Fatalf("findings = %+v, want only High", got)
	}
}

func TestAuditSuppressesDenseGrids(t *testing.T) {
	// A keyboard-like grid: 8 tiny same-size unlabeled buttons.
	keys := make([]*Node, 8)
	for i := range keys {
		keys[i] = anode("Button", "", float64(10+36*i), 700, 32, 42, true)
	}
	row := anode("Other", "", 0, 690, 393, 60, false, keys...)
	lone := anode("Button", "", 10, 100, 32, 42, true) // same size but not in a dense group
	cfg := auditConfig{minTouch: auditMinTouchDefault, alignTol: auditAlignTolDefault,
		spacingTol: auditSpacingTolDefault,
		checks:     map[string]bool{"touch_target": true, "missing_labels": true}}
	rep := runAudit([]*Node{row, lone}, auditViewport, cfg)
	if rep.suppressed != 8 {
		t.Fatalf("suppressed = %d, want 8", rep.suppressed)
	}
	if len(rep.findings) != 2 { // lone: one touch_target + one missing_labels
		t.Fatalf("findings = %+v, want only the lone button's 2", rep.findings)
	}
	for _, f := range rep.findings {
		if f.Element.Frame.Y != 100 {
			t.Fatalf("dense-grid member leaked into findings: %+v", f)
		}
	}
}

func TestAuditSmallGroupNotSuppressed(t *testing.T) {
	kids := make([]*Node, auditDenseGroupMin-1)
	for i := range kids {
		kids[i] = anode("Button", "", float64(10+50*i), 700, 32, 42, true)
	}
	rep := runAudit([]*Node{anode("Other", "", 0, 690, 393, 60, false, kids...)},
		auditViewport, auditOnly("touch_target"))
	if rep.suppressed != 0 || len(rep.findings) != auditDenseGroupMin-1 {
		t.Fatalf("suppressed = %d findings = %d, want group below threshold reported",
			rep.suppressed, len(rep.findings))
	}
}

func TestAuditRegionScope(t *testing.T) {
	cfg := auditOnly("touch_target")
	cfg.region = &Frame{X: 0, Y: 0, W: 393, H: 200}
	nodes := []*Node{
		anode("Button", "InRegion", 10, 100, 30, 30, true),
		anode("Button", "Outside", 10, 500, 30, 30, true),
	}
	rep := runAudit(nodes, auditViewport, cfg)
	got := findingsFor(rep, "touch_target")
	if len(got) != 1 || got[0].Element.Label != "InRegion" {
		t.Fatalf("findings = %+v, want only InRegion", got)
	}
}

func TestAuditRefsSequential(t *testing.T) {
	nodes := []*Node{
		anode("Button", "", 10, 100, 30, 30, true),
		anode("Button", "", 10, 200, 30, 30, true),
	}
	cfg := auditConfig{minTouch: auditMinTouchDefault, alignTol: auditAlignTolDefault,
		spacingTol: auditSpacingTolDefault}
	rep := runAudit(nodes, auditViewport, cfg)
	for i, f := range rep.findings {
		want := "F" + string(rune('1'+i))
		if f.Ref != want {
			t.Fatalf("ref[%d] = %q, want %q", i, f.Ref, want)
		}
	}
}

func TestAuditRejectsUnknownCheck(t *testing.T) {
	f := newFakeRunner()
	_, err := dispatch(t, testBackend(f), "audit", map[string]any{"checks": []any{"contrast"}})
	ae, ok := err.(*Error)
	if !ok || ae.Code != "bad_request" || !strings.Contains(ae.Message, "contrast") {
		t.Fatalf("want bad_request naming the unknown check, got %v", err)
	}
	if len(f.argvs()) != 0 {
		t.Fatalf("invalid request still ran commands: %q", f.argvs())
	}
}

func TestAuditRejectsBadRegion(t *testing.T) {
	f := newFakeRunner()
	_, err := dispatch(t, testBackend(f), "audit",
		map[string]any{"region": map[string]any{"x": 0, "y": 0, "w": -5, "h": 10}})
	ae, ok := err.(*Error)
	if !ok || ae.Code != "bad_request" {
		t.Fatalf("want bad_request for negative region size, got %v", err)
	}
}

// auditFixture is a canned describe-ui tree with one small unlabeled
// button and a clipped label, over a 393x852 screen.
const auditFixture = `[{"type":"Application","AXLabel":"","frame":{"x":0,"y":0,"width":393,"height":852},
  "children":[
    {"type":"Button","AXLabel":"","frame":{"x":16,"y":300,"width":30,"height":30}},
    {"type":"StaticText","AXLabel":"Wide title","frame":{"x":16,"y":100,"width":420,"height":20}}
  ]}]`

func auditTestPNG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff // white, opaque
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestAuditEndToEnd(t *testing.T) {
	f := newFakeRunner()
	f.stdout["describe-ui"] = auditFixture
	f.writeFile["screenshot"] = auditTestPNG(t, 393, 852)
	res, err := dispatch(t, testBackend(f), "audit", nil)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	findings := res.Result["findings"].([]auditFinding)
	checks := map[string]bool{}
	for _, f := range findings {
		checks[f.Check] = true
		if f.Ref == "" {
			t.Fatalf("finding without ref: %+v", f)
		}
	}
	// The tiny unlabeled button trips touch_target + missing_labels; the
	// wide title trips clipping.
	for _, want := range []string{"touch_target", "missing_labels", "clipping"} {
		if !checks[want] {
			t.Fatalf("checks hit = %v, want %s (findings %+v)", checks, want, findings)
		}
	}
	if res.Result["hash"] == "" {
		t.Fatal("missing tree hash")
	}
	b64, _ := res.Result["png_base64"].(string)
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("annotated image not base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("annotated image not PNG: %v", err)
	}
	// The button's box top-left corner (16,300 in points == pixels here)
	// must be painted annotation red.
	r, g, b, _ := img.At(17, 301).RGBA()
	if r>>8 != 220 || g>>8 != 30 || b>>8 != 30 {
		t.Fatalf("expected red annotation at (17,301), got rgb(%d,%d,%d)", r>>8, g>>8, b>>8)
	}
	if res.Result["format"] != "png" || res.Result["backend"] != "axe" {
		t.Fatalf("result meta = format:%v backend:%v", res.Result["format"], res.Result["backend"])
	}
}

func TestAuditMatcherScope(t *testing.T) {
	// Scope to the card: the small button outside it must not be audited.
	fixture := `[{"type":"Application","AXLabel":"","frame":{"x":0,"y":0,"width":393,"height":852},
	  "children":[
	    {"type":"Other","AXLabel":"Card","frame":{"x":0,"y":100,"width":393,"height":200},
	     "children":[{"type":"Button","AXLabel":"","frame":{"x":16,"y":120,"width":30,"height":30}}]},
	    {"type":"Button","AXLabel":"","frame":{"x":16,"y":500,"width":30,"height":30}}
	  ]}]`
	f := newFakeRunner()
	f.stdout["describe-ui"] = fixture
	f.writeFile["screenshot"] = auditTestPNG(t, 393, 852)
	res, err := dispatch(t, testBackend(f), "audit",
		map[string]any{"label": "Card", "checks": []any{"touch_target"}})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	findings := res.Result["findings"].([]auditFinding)
	if len(findings) != 1 || findings[0].Element.Frame.Y != 120 {
		t.Fatalf("findings = %+v, want only the card's button", findings)
	}
}

func TestAuditScreenshotFailureDegrades(t *testing.T) {
	f := newFakeRunner()
	f.stdout["describe-ui"] = auditFixture
	f.errs["screenshot"] = "boom"
	res, err := dispatch(t, testBackend(f), "audit", nil)
	if err != nil {
		t.Fatalf("audit should not fail on screenshot error: %v", err)
	}
	if msg, _ := res.Result["screenshot_error"].(string); msg == "" {
		t.Fatal("missing screenshot_error")
	}
	if len(res.Result["findings"].([]auditFinding)) == 0 {
		t.Fatal("findings should survive a screenshot failure")
	}
	if _, ok := res.Result["png_base64"]; ok {
		t.Fatal("no image should be returned on screenshot failure")
	}
}
