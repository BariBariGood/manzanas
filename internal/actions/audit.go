package actions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/BariBariGood/manzanas/internal/imgutil"
)

// The audit action runs deterministic UI-quality checks over one
// accessibility-tree observation and the matching screenshot, producing
// EVIDENCE (findings with measured values), never pass/fail verdicts.
// The judgment stays with the agent or human reading the findings.

// Audit defaults and noise bounds.
const (
	// auditMinTouchDefault is the Apple HIG minimum touch target (points).
	auditMinTouchDefault = 44
	// auditAlignTolDefault is the near-miss alignment window: edge deltas
	// in (auditEdgeSlack, tol] are flagged; larger deltas are treated as
	// intentional layout.
	auditAlignTolDefault = 4
	// auditSpacingTolDefault is how far a sibling gap may deviate from
	// the group's median before it is flagged (points).
	auditSpacingTolDefault = 4
	// auditSpacingMinGaps is the minimum number of gaps a sibling sequence
	// needs before its median is meaningful: with only two gaps the median
	// is their average, so any unequal pair deviates and flags both.
	auditSpacingMinGaps = 3
	// auditEdgeSlack absorbs sub-point float jitter in AXe frames.
	auditEdgeSlack = 0.5
	// auditDenseGroupMin is the sibling-group size at which same-role,
	// same-size controls are treated as a dense repeated grid (keyboard
	// keys, emoji grids, calendar day cells) and suppressed.
	auditDenseGroupMin = 6
	// auditMaxPerCheck caps findings per check; the rest are counted as
	// elided so one pathological screen cannot flood the report.
	auditMaxPerCheck = 20
	// auditAlignGroupCap skips pairwise alignment comparison in sibling
	// groups larger than this (dense layouts are grids, not near-misses).
	auditAlignGroupCap = 12
)

// auditCheckNames is the canonical check order (also the docs order).
var auditCheckNames = []string{
	"touch_target", "clipping", "alignment", "spacing", "safe_area", "missing_labels",
}

// scrollContainerRoles legitimately clip their children; child frames
// extending beyond them are scroll content, not layout bugs.
var scrollContainerRoles = map[string]bool{
	"ScrollView": true, "Table": true, "CollectionView": true, "List": true,
	"WebView": true,
}

// auditElement identifies one element in a finding.
type auditElement struct {
	Role  string `json:"role,omitempty"`
	Label string `json:"label,omitempty"`
	ID    string `json:"id,omitempty"`
	Frame *Frame `json:"frame,omitempty"`
}

// auditFinding is one piece of evidence: the check that produced it, the
// element (and any related elements) it concerns, the measured values,
// and a human/agent-readable evidence sentence. Ref ("F1", "F2", ...)
// matches the label drawn on the annotated screenshot.
type auditFinding struct {
	Check    string         `json:"check"`
	Ref      string         `json:"ref"`
	Element  auditElement   `json:"element"`
	Related  []auditElement `json:"related,omitempty"`
	Measured map[string]any `json:"measured,omitempty"`
	Evidence string         `json:"evidence"`
}

// auditInsets are safe-area insets in points.
type auditInsets struct {
	Top, Bottom, Left, Right float64
}

// auditConfig carries the parsed audit payload knobs.
type auditConfig struct {
	checks     map[string]bool // nil = all checks
	minTouch   float64
	alignTol   float64
	spacingTol float64
	insets     *auditInsets // nil = heuristic from viewport
	region     *Frame
	// includeChrome re-includes system chrome (scroll indicators, status
	// bar, keyboard — see chrome.go) that is suppressed by default.
	includeChrome bool
	// includeCovered re-includes interactive controls whose touch target
	// is effectively provided by an enclosing >=min-size tappable list
	// row (see coveredByRow); suppressed from touch_target by default.
	includeCovered bool
}

// auditReport is what runAudit produces.
type auditReport struct {
	findings   []auditFinding
	counts     map[string]int
	elided     map[string]int
	suppressed int
	covered    int // touch_target findings withheld as row-covered
}

// screenCapturer captures the target's screen as PNG; implemented by the
// simulator backend, faked in tests.
type screenCapturer interface {
	capturePNG(ctx context.Context, udid string) (img []byte, backend string, err error)
}

func handleAudit(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return elemAudit(ctx, b, b, udid, p)
}

// elemAudit observes the tree, runs the configured checks over the
// (optionally matcher/region-scoped) elements, captures a screenshot, and
// annotates every finding's frame on it.
func elemAudit(ctx context.Context, d elementDriver, cap screenCapturer, udid string, p map[string]any) (map[string]any, error) {
	cfg, err := auditConfigFromPayload(p)
	if err != nil {
		return nil, err
	}
	// inline is consumed by the server (which strips the base64 payload
	// from the wire response); validate it up front like screenshot does.
	if _, err := boolFlag(p, "inline", true); err != nil {
		return nil, err
	}
	timeout, interval, err := waitParams(p, defaultWaitTimeout)
	if err != nil {
		return nil, err
	}
	obs, err := observeReadable(ctx, d, udid, false, time.Now().Add(timeout), interval)
	if err != nil {
		if errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded) {
			return nil, timeoutErr("no readable accessibility snapshot within the %s budget; is an app in the foreground?", timeout)
		}
		return nil, err
	}
	scope := obs.nodes
	if auditHasMatcher(p) {
		m, err := matcherFromPayload(p)
		if err != nil {
			return nil, err
		}
		hit, rerr := m.resolve(obs.nodes, obs.viewport)
		if rerr != nil {
			if errors.Is(rerr, errNoMatch) {
				return nil, timeoutErr("audit scope (%s) matched no element%s; call observe/ui_tree to see what is on screen and adjust the matcher",
					m, offscreenHint(m, obs.nodes, obs.viewport))
			}
			return nil, rerr
		}
		scope = []*Node{hit}
	}
	// The parent map always comes from the full tree so ancestor-dependent
	// checks (scroll-container exemptions) keep working under a scoped audit.
	rep := runAuditWithParents(scope, parentMap(obs.nodes), obs.viewport, cfg)
	if rep.findings == nil {
		rep.findings = []auditFinding{}
	}
	res := map[string]any{
		"findings":            rep.findings,
		"counts":              rep.counts,
		"suppressed_elements": rep.suppressed,
		"hash":                TreeHash(obs.nodes),
	}
	if rep.covered > 0 {
		res["suppressed_covered_controls"] = rep.covered
	}
	if len(rep.elided) > 0 {
		res["elided"] = rep.elided
	}
	if obs.viewport != nil {
		res["viewport"] = obs.viewport
	}
	annotateAudit(ctx, cap, udid, obs.viewport, rep.findings, res)
	return res, nil
}

// annotateAudit captures the screen and draws a labeled red box around
// every finding's frame. Best-effort: a capture or annotation failure
// degrades to a screenshot_error field — the findings are still evidence.
func annotateAudit(ctx context.Context, cap screenCapturer, udid string, viewport *Frame, findings []auditFinding, res map[string]any) {
	img, backend, err := cap.capturePNG(ctx, udid)
	if err != nil {
		res["screenshot_error"] = err.Error()
		return
	}
	// Element frames are in points; the capture is native pixels (2x/3x).
	// Without a viewport the point→pixel factor is unknown and boxes would
	// land in the wrong place, so skip the overlay rather than mislead.
	if viewport == nil || viewport.W <= 0 {
		res["screenshot_error"] = "annotation skipped: viewport unknown, cannot map point frames onto the capture"
		return
	}
	scale := 1.0
	if cfgImg, _, err := image.DecodeConfig(bytes.NewReader(img)); err == nil && cfgImg.Width > 0 {
		scale = float64(cfgImg.Width) / viewport.W
	} else {
		res["screenshot_error"] = "annotation skipped: could not decode the capture's dimensions"
		return
	}
	boxes := make([]imgutil.Box, 0, len(findings))
	for _, f := range findings {
		fr := f.Element.Frame
		if fr == nil {
			continue
		}
		boxes = append(boxes, imgutil.Box{
			X: int(math.Round(fr.X * scale)), Y: int(math.Round(fr.Y * scale)),
			W: int(math.Round(fr.W * scale)), H: int(math.Round(fr.H * scale)),
			Label: f.Ref,
		})
	}
	annotated, err := imgutil.Annotate(img, boxes)
	if err != nil {
		res["screenshot_error"] = fmt.Sprintf("annotate failed: %v", err)
		return
	}
	sum := sha256.Sum256(annotated)
	res["format"] = "png"
	res["png_base64"] = base64.StdEncoding.EncodeToString(annotated)
	res["bytes"] = len(annotated)
	res["sha256"] = hex.EncodeToString(sum[:])
	res["backend"] = backend
}

// auditHasMatcher reports whether the payload carries any element-matcher
// field (flat or predicate DSL) that scopes the audit.
func auditHasMatcher(p map[string]any) bool {
	for _, k := range []string{"label", "role", "value", "id", "placeholder"} {
		if strField(p, k) != "" {
			return true
		}
	}
	if v, ok := p["predicate"]; ok && v != nil {
		return true
	}
	return false
}

// auditConfigFromPayload parses the audit knobs, rejecting unknown check
// names and malformed values explicitly.
func auditConfigFromPayload(p map[string]any) (auditConfig, error) {
	cfg := auditConfig{
		minTouch:   auditMinTouchDefault,
		alignTol:   auditAlignTolDefault,
		spacingTol: auditSpacingTolDefault,
	}
	var err error
	if cfg.includeChrome, err = boolFlag(p, "include_system_chrome", false); err != nil {
		return cfg, err
	}
	if cfg.includeCovered, err = boolFlag(p, "include_covered_controls", false); err != nil {
		return cfg, err
	}
	if raw, ok := p["checks"]; ok {
		list, ok := raw.([]any)
		if !ok || len(list) == 0 {
			return cfg, badRequest("payload field %q must be a non-empty array of check names (%s)", "checks", strings.Join(auditCheckNames, ", "))
		}
		cfg.checks = map[string]bool{}
		for _, v := range list {
			name, ok := v.(string)
			if !ok || !auditCheckKnown(name) {
				return cfg, badRequest("unknown audit check %q (known checks: %s)", v, strings.Join(auditCheckNames, ", "))
			}
			cfg.checks[name] = true
		}
	}
	for _, kv := range []struct {
		key string
		dst *float64
	}{{"min_touch_pt", &cfg.minTouch}, {"alignment_tolerance_pt", &cfg.alignTol}, {"spacing_tolerance_pt", &cfg.spacingTol}} {
		if v, ok := p[kv.key]; ok {
			n, err := toNum(v)
			if err != nil || n <= 0 {
				return cfg, badRequest("payload field %q must be a positive number of points", kv.key)
			}
			*kv.dst = n
		}
	}
	if raw, ok := p["safe_area_insets"]; ok {
		m, ok := raw.(map[string]any)
		if !ok {
			return cfg, badRequest("payload field %q must be an object with top, bottom, left, right (points)", "safe_area_insets")
		}
		ins := auditInsets{}
		for _, kv := range []struct {
			key string
			dst *float64
		}{{"top", &ins.Top}, {"bottom", &ins.Bottom}, {"left", &ins.Left}, {"right", &ins.Right}} {
			if v, ok := m[kv.key]; ok {
				n, err := toNum(v)
				if err != nil || n < 0 {
					return cfg, badRequest("safe_area_insets field %q must be a non-negative number of points", kv.key)
				}
				*kv.dst = n
			}
		}
		cfg.insets = &ins
	}
	if raw, ok := p["region"]; ok {
		m, ok := raw.(map[string]any)
		if !ok {
			return cfg, badRequest("payload field %q must be an object with x, y, w, h (points)", "region")
		}
		f := Frame{}
		for _, kv := range []struct {
			key string
			dst *float64
		}{{"x", &f.X}, {"y", &f.Y}, {"w", &f.W}, {"h", &f.H}} {
			v, err := numField(m, kv.key)
			if err != nil {
				return cfg, badRequest("region field %q must be a number", kv.key)
			}
			*kv.dst = v
		}
		if f.W <= 0 || f.H <= 0 {
			return cfg, badRequest("region requires positive w and h")
		}
		cfg.region = &f
	}
	return cfg, nil
}

func auditCheckKnown(name string) bool {
	for _, c := range auditCheckNames {
		if c == name {
			return true
		}
	}
	return false
}

func (c auditConfig) enabled(check string) bool {
	return c.checks == nil || c.checks[check]
}

// auditState is the shared per-run context the checks read.
type auditState struct {
	cfg        auditConfig
	viewport   *Frame
	parents    map[*Node]*Node
	visible    []*Node           // DFS order, valid frame, in viewport/region
	suppressed map[*Node]bool    // dense repeated controls + system chrome
	covered    int               // touch_target candidates withheld as row-covered
	byParent   map[*Node][]*Node // visible children per parent (nil key = roots)
	parentSeq  []*Node           // parents in first-seen DFS order (deterministic iteration)
}

// siblingGroups yields the visible sibling groups in first-seen DFS order,
// so findings (and their F-refs) are reproducible across runs.
func (st *auditState) siblingGroups() [][]*Node {
	out := make([][]*Node, 0, len(st.parentSeq))
	for _, p := range st.parentSeq {
		out = append(out, st.byParent[p])
	}
	return out
}

// runAudit executes every enabled check over the given tree.
func runAudit(nodes []*Node, viewport *Frame, cfg auditConfig) auditReport {
	return runAuditWithParents(nodes, parentMap(nodes), viewport, cfg)
}

// runAuditWithParents runs the checks over nodes while resolving ancestry
// through parents, which may span a larger tree than the audited scope.
func runAuditWithParents(nodes []*Node, parents map[*Node]*Node, viewport *Frame, cfg auditConfig) auditReport {
	st := &auditState{
		cfg: cfg, viewport: viewport,
		parents:    parents,
		suppressed: map[*Node]bool{},
		byParent:   map[*Node][]*Node{},
	}
	walkNodes(nodes, func(n *Node) {
		if !st.visibleNode(n) {
			return
		}
		st.visible = append(st.visible, n)
		p := st.parents[n]
		if _, seen := st.byParent[p]; !seen {
			st.parentSeq = append(st.parentSeq, p)
		}
		st.byParent[p] = append(st.byParent[p], n)
	})
	st.markDenseGroups()
	st.markSystemChrome()

	rep := auditReport{counts: map[string]int{}, elided: map[string]int{}}
	add := func(fs []auditFinding) {
		if len(fs) == 0 {
			return
		}
		check := fs[0].Check
		kept := fs
		if len(kept) > auditMaxPerCheck {
			rep.elided[check] = len(kept) - auditMaxPerCheck
			kept = kept[:auditMaxPerCheck]
		}
		rep.counts[check] = len(kept)
		rep.findings = append(rep.findings, kept...)
	}
	if cfg.enabled("touch_target") {
		add(st.checkTouchTargets())
	}
	if cfg.enabled("clipping") {
		add(st.checkClipping())
	}
	if cfg.enabled("alignment") {
		add(st.checkAlignment())
	}
	if cfg.enabled("spacing") {
		add(st.checkSpacing())
	}
	if cfg.enabled("safe_area") {
		add(st.checkSafeArea())
	}
	if cfg.enabled("missing_labels") {
		add(st.checkMissingLabels())
	}
	for i := range rep.findings {
		rep.findings[i].Ref = fmt.Sprintf("F%d", i+1)
	}
	rep.suppressed = len(st.suppressed)
	rep.covered = st.covered
	if len(rep.elided) == 0 {
		rep.elided = nil
	}
	return rep
}

// visibleNode reports whether a node participates in the audit: a valid
// frame that intersects the viewport (offscreen scroll content is not
// what the user sees) and, when a region is set, whose centre lies in it.
func (st *auditState) visibleNode(n *Node) bool {
	f := n.Frame
	if f == nil || f.W <= 0 || f.H <= 0 {
		return false
	}
	if v := st.viewport; v != nil {
		if f.X+f.W <= v.X || f.X >= v.X+v.W || f.Y+f.H <= v.Y || f.Y >= v.Y+v.H {
			return false
		}
	}
	if r := st.cfg.region; r != nil {
		cx, cy := f.Center()
		if cx < r.X || cx > r.X+r.W || cy < r.Y || cy > r.Y+r.H {
			return false
		}
	}
	return true
}

// markDenseGroups suppresses dense grids of repeated same-role,
// same-size sibling controls (keyboards, emoji grids, calendar day
// cells): each key failing touch_target or missing_labels individually
// would be noise, not evidence.
func (st *auditState) markDenseGroups() {
	for _, siblings := range st.siblingGroups() {
		groups := map[string][]*Node{}
		for _, n := range siblings {
			key := fmt.Sprintf("%s|%.0f|%.0f", n.Role, n.Frame.W/4, n.Frame.H/4)
			groups[key] = append(groups[key], n)
		}
		for _, g := range groups {
			if len(g) >= auditDenseGroupMin {
				for _, n := range g {
					st.suppressed[n] = true
				}
			}
		}
	}
	for _, n := range st.visible {
		if n.Role == "Key" {
			st.suppressed[n] = true
		}
	}
}

// markSystemChrome suppresses OS-drawn elements (scroll-indicator
// pseudo-elements, the status bar, the keyboard — heuristics in
// chrome.go): they are Apple's chrome, not the app's layout, so their
// geometry is noise on every stock screen. Default-on; disable with
// include_system_chrome:true.
func (st *auditState) markSystemChrome() {
	if st.cfg.includeChrome {
		return
	}
	for _, n := range st.visible {
		if !st.suppressed[n] && isSystemChrome(n, st.parents) {
			st.suppressed[n] = true
		}
	}
}

// coveredByRow reports whether an interactive >=min-size Cell or Button
// ancestor (a tappable list row) fully contains the element's frame:
// stock UIKit list rows (e.g. Settings, which surfaces rows as
// full-width Buttons on iOS 26) expose small (~28pt) label Buttons and
// chevrons inside a >=44pt tappable row, and the effective touch target
// is the whole row. Flagging the inner control is noise; disable the
// suppression with include_covered_controls:true.
func (st *auditState) coveredByRow(n *Node, min float64) bool {
	for p := st.parents[n]; p != nil; p = st.parents[p] {
		f := p.Frame
		if (p.Role != "Cell" && p.Role != "Button") || !p.Interactable || f == nil {
			continue
		}
		if f.W < min-auditEdgeSlack || f.H < min-auditEdgeSlack {
			continue
		}
		if frameContainsFrame(f, n.Frame, 1) {
			return true
		}
	}
	return false
}

// frameContainsFrame reports whether inner lies entirely inside outer,
// with tol points of slack per edge.
func frameContainsFrame(outer, inner *Frame, tol float64) bool {
	return inner.X >= outer.X-tol && inner.Y >= outer.Y-tol &&
		inner.X+inner.W <= outer.X+outer.W+tol &&
		inner.Y+inner.H <= outer.Y+outer.H+tol
}

func auditElem(n *Node) auditElement {
	return auditElement{Role: n.Role, Label: n.Label, ID: n.Identifier, Frame: n.Frame}
}

// elemName renders a short human reference for evidence sentences.
func elemName(n *Node) string {
	switch {
	case n.Label != "":
		return fmt.Sprintf("%s %q", orRole(n.Role), n.Label)
	case n.Identifier != "":
		return fmt.Sprintf("%s id=%q", orRole(n.Role), n.Identifier)
	default:
		return fmt.Sprintf("unlabeled %s at (%s,%s)", orRole(n.Role), fmtNum(n.Frame.X), fmtNum(n.Frame.Y))
	}
}

func orRole(role string) string {
	if role == "" {
		return "element"
	}
	return role
}

// checkTouchTargets flags interactive elements smaller than the minimum
// touch target in either dimension.
func (st *auditState) checkTouchTargets() []auditFinding {
	var out []auditFinding
	min := st.cfg.minTouch
	for _, n := range st.visible {
		if !n.Interactable || st.suppressed[n] {
			continue
		}
		f := n.Frame
		if f.W >= min-auditEdgeSlack && f.H >= min-auditEdgeSlack {
			continue
		}
		if !st.cfg.includeCovered && st.coveredByRow(n, min) {
			st.covered++
			continue
		}
		out = append(out, auditFinding{
			Check:   "touch_target",
			Element: auditElem(n),
			Measured: map[string]any{
				"w_pt": f.W, "h_pt": f.H, "min_pt": min,
			},
			Evidence: fmt.Sprintf("interactive %s has a %sx%spt frame; smaller than the %sx%spt minimum touch target",
				elemName(n), fmtNum(f.W), fmtNum(f.H), fmtNum(min), fmtNum(min)),
		})
	}
	return out
}

// checkClipping flags frames extending beyond the screen bounds or beyond
// a non-scrolling parent's bounds.
func (st *auditState) checkClipping() []auditFinding {
	var out []auditFinding
	for _, n := range st.visible {
		if st.suppressed[n] {
			continue
		}
		if st.viewport != nil && !st.inScrollContainer(n) {
			if over := frameOverflow(n.Frame, st.viewport, auditEdgeSlack); len(over) > 0 {
				out = append(out, auditFinding{
					Check:   "clipping",
					Element: auditElem(n),
					Measured: map[string]any{
						"bounds": "screen", "overflow_pt": over,
					},
					Evidence: fmt.Sprintf("%s extends beyond the screen bounds by %s",
						elemName(n), overflowText(over)),
				})
			}
		}
		p := st.parents[n]
		if p == nil || p.Frame == nil || p.Frame.W <= 0 || p.Frame.H <= 0 ||
			scrollContainerRoles[p.Role] {
			continue
		}
		if over := frameOverflow(n.Frame, p.Frame, 1); len(over) > 0 {
			out = append(out, auditFinding{
				Check:   "clipping",
				Element: auditElem(n),
				Related: []auditElement{auditElem(p)},
				Measured: map[string]any{
					"bounds": "parent", "overflow_pt": over,
				},
				Evidence: fmt.Sprintf("%s extends beyond its parent %s by %s",
					elemName(n), elemName(p), overflowText(over)),
			})
		}
	}
	return out
}

// inScrollContainer reports whether any ancestor is a scrolling
// container; its descendants extending past the screen edge is normal
// scroll content, not clipping.
func (st *auditState) inScrollContainer(n *Node) bool {
	for p := st.parents[n]; p != nil; p = st.parents[p] {
		if scrollContainerRoles[p.Role] {
			return true
		}
	}
	return false
}

// frameOverflow measures how far f extends beyond bounds on each edge
// (positive points only), ignoring overflow within tol.
func frameOverflow(f, bounds *Frame, tol float64) map[string]float64 {
	over := map[string]float64{}
	if d := bounds.X - f.X; d > tol {
		over["left"] = round1(d)
	}
	if d := bounds.Y - f.Y; d > tol {
		over["top"] = round1(d)
	}
	if d := (f.X + f.W) - (bounds.X + bounds.W); d > tol {
		over["right"] = round1(d)
	}
	if d := (f.Y + f.H) - (bounds.Y + bounds.H); d > tol {
		over["bottom"] = round1(d)
	}
	if len(over) == 0 {
		return nil
	}
	return over
}

func overflowText(over map[string]float64) string {
	keys := make([]string, 0, len(over))
	for k := range over {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%spt (%s)", fmtNum(over[k]), k))
	}
	return strings.Join(parts, ", ")
}

// checkAlignment flags near-miss edge alignment between sibling elements:
// edges that almost — but not quite — line up within the tolerance, which
// reads as intended alignment that slipped.
func (st *auditState) checkAlignment() []auditFinding {
	var out []auditFinding
	tol := st.cfg.alignTol
	for _, siblings := range st.siblingGroups() {
		cands := alignCandidates(siblings, st.suppressed)
		if len(cands) < 2 || len(cands) > auditAlignGroupCap {
			continue
		}
		for i := 0; i < len(cands); i++ {
			for j := i + 1; j < len(cands); j++ {
				a, b := cands[i].Frame, cands[j].Frame
				stacked := !spansOverlap(a.Y, a.H, b.Y, b.H) && spansOverlap(a.X, a.W, b.X, b.W)
				sideBySide := !spansOverlap(a.X, a.W, b.X, b.W) && spansOverlap(a.Y, a.H, b.Y, b.H)
				var edges [][2]float64
				var names []string
				switch {
				case stacked:
					edges = [][2]float64{{a.X, b.X}, {a.X + a.W, b.X + b.W}}
					names = []string{"left", "right"}
				case sideBySide:
					edges = [][2]float64{{a.Y, b.Y}, {a.Y + a.H, b.Y + b.H}}
					names = []string{"top", "bottom"}
				default:
					continue
				}
				for k, e := range edges {
					d := math.Abs(e[0] - e[1])
					if d <= auditEdgeSlack || d > tol {
						continue
					}
					out = append(out, auditFinding{
						Check:   "alignment",
						Element: auditElem(cands[i]),
						Related: []auditElement{auditElem(cands[j])},
						Measured: map[string]any{
							"edge": names[k], "delta_pt": round1(d), "tolerance_pt": tol,
						},
						Evidence: fmt.Sprintf("%s edges of %s and %s are %spt apart — almost but not quite aligned",
							names[k], elemName(cands[i]), elemName(cands[j]), fmtNum(round1(d))),
					})
				}
			}
		}
	}
	return out
}

// alignCandidates keeps the siblings worth comparing: labeled or
// interactive, not suppressed.
func alignCandidates(siblings []*Node, suppressed map[*Node]bool) []*Node {
	var out []*Node
	for _, n := range siblings {
		if suppressed[n] || (!n.Interactable && n.Label == "") {
			continue
		}
		out = append(out, n)
	}
	return out
}

// checkSpacing flags inconsistent gaps in sequences of sibling elements
// laid out along one axis: gaps deviating from the group's median by more
// than the tolerance.
func (st *auditState) checkSpacing() []auditFinding {
	var out []auditFinding
	tol := st.cfg.spacingTol
	for _, siblings := range st.siblingGroups() {
		cands := alignCandidates(siblings, st.suppressed)
		if len(cands) < 3 {
			continue
		}
		seq, axis := sequenceAlong(cands)
		if seq == nil || len(seq)-1 < auditSpacingMinGaps {
			continue
		}
		gaps := make([]float64, len(seq)-1)
		for i := 1; i < len(seq); i++ {
			if axis == "column" {
				gaps[i-1] = seq[i].Frame.Y - (seq[i-1].Frame.Y + seq[i-1].Frame.H)
			} else {
				gaps[i-1] = seq[i].Frame.X - (seq[i-1].Frame.X + seq[i-1].Frame.W)
			}
		}
		med := median(gaps)
		for i, g := range gaps {
			if math.Abs(g-med) <= tol {
				continue
			}
			out = append(out, auditFinding{
				Check:   "spacing",
				Element: auditElem(seq[i+1]),
				Related: []auditElement{auditElem(seq[i])},
				Measured: map[string]any{
					"axis": axis, "gap_pt": round1(g), "median_gap_pt": round1(med),
					"gaps_pt": roundAll(gaps), "tolerance_pt": tol,
				},
				Evidence: fmt.Sprintf("%s gap between %s and %s is %spt; the sibling sequence's median gap is %spt (gaps: %v)",
					axis, elemName(seq[i]), elemName(seq[i+1]), fmtNum(round1(g)), fmtNum(round1(med)), roundAll(gaps)),
			})
		}
	}
	return out
}

// sequenceAlong orders siblings into a non-overlapping column or row
// sequence, or reports that they form neither.
func sequenceAlong(cands []*Node) ([]*Node, string) {
	col := append([]*Node(nil), cands...)
	sort.SliceStable(col, func(i, j int) bool { return col[i].Frame.Y < col[j].Frame.Y })
	if sequential(col, func(prev, next *Frame) bool { return next.Y >= prev.Y+prev.H-1 }) {
		return col, "column"
	}
	row := append([]*Node(nil), cands...)
	sort.SliceStable(row, func(i, j int) bool { return row[i].Frame.X < row[j].Frame.X })
	if sequential(row, func(prev, next *Frame) bool { return next.X >= prev.X+prev.W-1 }) {
		return row, "row"
	}
	return nil, ""
}

func sequential(seq []*Node, ok func(prev, next *Frame) bool) bool {
	for i := 1; i < len(seq); i++ {
		if !ok(seq[i-1].Frame, seq[i].Frame) {
			return false
		}
	}
	return true
}

func median(vals []float64) float64 {
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// checkSafeArea flags interactive elements intruding into the safe-area
// insets. Insets come from the payload or a device heuristic; full-bleed
// backgrounds are exempt.
func (st *auditState) checkSafeArea() []auditFinding {
	v := st.viewport
	if v == nil {
		return nil
	}
	ins := st.cfg.insets
	if ins == nil {
		if v.H >= 700 {
			// Notch/Dynamic Island device classes: status region + home
			// indicator.
			ins = &auditInsets{Top: 59, Bottom: 34}
		} else {
			ins = &auditInsets{Top: 20}
		}
	}
	var out []auditFinding
	for _, n := range st.visible {
		if !n.Interactable || st.suppressed[n] {
			continue
		}
		f := n.Frame
		if f.W >= 0.95*v.W && f.H >= 0.9*v.H {
			continue // full-bleed container, not a control in the inset
		}
		for _, band := range []struct {
			edge    string
			overlap float64
		}{
			{"top", ins.Top - f.Y},
			{"bottom", (f.Y + f.H) - (v.H - ins.Bottom)},
			{"left", ins.Left - f.X},
			{"right", (f.X + f.W) - (v.W - ins.Right)},
		} {
			depth := band.overlap
			switch band.edge {
			case "top":
				if ins.Top <= 0 {
					continue
				}
				depth = math.Min(depth, f.H)
			case "bottom":
				if ins.Bottom <= 0 {
					continue
				}
				depth = math.Min(depth, f.H)
			case "left":
				if ins.Left <= 0 {
					continue
				}
				depth = math.Min(depth, f.W)
			case "right":
				if ins.Right <= 0 {
					continue
				}
				depth = math.Min(depth, f.W)
			}
			if depth <= auditEdgeSlack {
				continue
			}
			out = append(out, auditFinding{
				Check:   "safe_area",
				Element: auditElem(n),
				Measured: map[string]any{
					"edge": band.edge, "overlap_pt": round1(depth),
					"insets_pt": map[string]float64{"top": ins.Top, "bottom": ins.Bottom, "left": ins.Left, "right": ins.Right},
				},
				Evidence: fmt.Sprintf("interactive %s overlaps the %s safe-area inset by %spt",
					elemName(n), band.edge, fmtNum(round1(depth))),
			})
		}
	}
	return out
}

// checkMissingLabels flags interactive elements with no accessibility
// label (no label, value, or placeholder — nothing a screen reader or an
// agent matcher can use).
func (st *auditState) checkMissingLabels() []auditFinding {
	var out []auditFinding
	for _, n := range st.visible {
		if !n.Interactable || st.suppressed[n] {
			continue
		}
		if n.Label != "" || n.Value != "" || n.Placeholder != "" {
			continue
		}
		measured := map[string]any{"role": n.Role}
		detail := ""
		if n.Identifier != "" {
			measured["id"] = n.Identifier
			detail = fmt.Sprintf(" (identifier %q is set but identifiers are not read to assistive tech)", n.Identifier)
		}
		out = append(out, auditFinding{
			Check:    "missing_labels",
			Element:  auditElem(n),
			Measured: measured,
			Evidence: fmt.Sprintf("interactive %s at (%s,%s %sx%s) has no accessibility label, value, or placeholder%s",
				orRole(n.Role), fmtNum(n.Frame.X), fmtNum(n.Frame.Y), fmtNum(n.Frame.W), fmtNum(n.Frame.H), detail),
		})
	}
	return out
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func roundAll(vals []float64) []float64 {
	out := make([]float64, len(vals))
	for i, v := range vals {
		out[i] = round1(v)
	}
	return out
}
