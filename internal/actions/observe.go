package actions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// observeRetries is how many times describe-ui is retried when the
// simulator's accessibility bridge is not ready yet ("No translation object
// returned" right after app launch). The bridge can take ~20s to attach on
// a cold simulator, so the budget is generous.
const observeRetries = 8

// observeBackoff is the base delay between describe-ui retries; it grows
// linearly with the attempt number (total budget ~27s).
var observeBackoff = 750 * time.Millisecond

// emptyTreeRetries bounds how many times an empty-but-parseable tree is
// re-polled before it is returned as a legitimately element-free screen.
const emptyTreeRetries = 2

// Node is one compacted accessibility element. Zero-valued fields are
// omitted so snapshots stay token-cheap for LLM callers.
type Node struct {
	Role         string  `json:"role,omitempty"`
	Label        string  `json:"label,omitempty"`
	Value        string  `json:"value,omitempty"`
	Placeholder  string  `json:"placeholder,omitempty"`
	Identifier   string  `json:"id,omitempty"`
	Frame        *Frame  `json:"frame,omitempty"`
	Interactable bool    `json:"interactable,omitempty"`
	Disabled     bool    `json:"disabled,omitempty"`
	Children     []*Node `json:"children,omitempty"`
}

// Frame is an element's screen rectangle in points.
type Frame struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Center returns the frame's midpoint, the natural tap target.
func (f Frame) Center() (float64, float64) { return f.X + f.W/2, f.Y + f.H/2 }

func handleObserve(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	includeRaw, err := boolFlag(p, "include_raw", false)
	if err != nil {
		return nil, err
	}
	// refresh changes nothing on the cold path (each observe already
	// spawns a fresh AXe process); it is validated here so a malformed
	// value fails consistently whichever backend serves the observe.
	if _, err := boolFlag(p, "refresh", false); err != nil {
		return nil, err
	}
	raw, nodes, err := b.observeTree(ctx, udid)
	if err != nil {
		return nil, err
	}
	if nodes == nil {
		nodes = []*Node{}
	}
	res := map[string]any{
		"tree": nodes,
		"hash": TreeHash(nodes),
	}
	if len(nodes) == 0 {
		// The tree stayed empty across the bounded in-daemon re-polls.
		// That is either a legitimately element-free screen or a still-
		// warming RN a11y bridge; the distinct detail lets callers treat
		// it as retryable instead of trusting an empty snapshot.
		res["detail"] = "empty_tree"
	}
	if includeRaw {
		res["raw"] = string(raw)
	}
	return res, nil
}

// observeTree runs `axe describe-ui` and compacts it, retrying with
// backoff while the accessibility bridge is still attaching: right after a
// launch it either errors ("No translation object returned") or returns a
// skeleton tree that compacts to nothing.
func (b *AXeBackend) observeTree(ctx context.Context, udid string) ([]byte, []*Node, error) {
	var lastErr error
	empties := 0
	for attempt := 0; attempt < observeRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * observeBackoff):
			}
		}
		raw, err := b.axe(ctx, udid, "describe-ui")
		if err != nil {
			lastErr = err
			if !isTransientA11yError(err) {
				return nil, nil, err
			}
			continue
		}
		nodes, err := CompactTree(raw)
		if err != nil {
			// Non-JSON stdout with a zero exit status is another form of
			// the bridge not being ready yet; keep polling.
			lastErr = err
			continue
		}
		if len(nodes) > 0 || empties >= emptyTreeRetries {
			// A tree that stays empty across a few polls is a real
			// (legitimately element-free) screen, not a race.
			return raw, nodes, nil
		}
		empties++
	}
	// The budget was exhausted on a transient condition: the bridge may
	// still attach, so signal "retry later" rather than a hard failure.
	if e, ok := lastErr.(*Error); ok && e.Code == "internal" {
		return nil, nil, unavailable("accessibility bridge not ready after %d attempts; retry later: %s", observeRetries, e.Message)
	}
	return nil, nil, lastErr
}

// boolFlag extracts an optional boolean payload field, rejecting non-bool
// values instead of silently coercing them to the default.
func boolFlag(p map[string]any, key string, def bool) (bool, error) {
	v, ok := p[key]
	if !ok {
		return def, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, badRequest("payload field %q must be a boolean", key)
	}
	return b, nil
}

// isTransientA11yError reports whether the failure is the known
// post-launch race where the a11y bridge is not yet attached.
func isTransientA11yError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no translation object") ||
		strings.Contains(msg, "not ready") ||
		strings.Contains(msg, "timed out")
}

// CompactTree parses `axe describe-ui` JSON into a compact node tree:
// noise-only wrappers are collapsed and empty containers dropped, so the
// result is cheap to feed to an agent.
func CompactTree(raw []byte) ([]*Node, error) {
	var root any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, internal("describe-ui output is not valid JSON: %v", err)
	}
	return dedupeSiblings(compactAny(root)), nil
}

func compactAny(v any) []*Node {
	switch t := v.(type) {
	case []any:
		var out []*Node
		for _, e := range t {
			out = append(out, compactAny(e)...)
		}
		return out
	case map[string]any:
		return compactObject(t)
	}
	return nil
}

// compactObject converts one describe-ui element, collapsing it into its
// children when the element itself carries no information.
func compactObject(m map[string]any) []*Node {
	n := &Node{
		Role:        firstString(m, "role", "type", "AXType"),
		Label:       firstString(m, "AXLabel", "label", "title", "AXTitle"),
		Value:       firstString(m, "AXValue", "value"),
		Placeholder: firstString(m, "AXPlaceholderValue", "placeholder"),
		Identifier:  firstString(m, "AXUniqueId", "identifier", "AXIdentifier"),
		Frame:       parseFrame(m),
	}
	if rd := firstString(m, "role_description"); n.Role == "" && rd != "" {
		n.Role = rd
	}
	n.Role = normalizeRole(n.Role)
	n.Interactable = isInteractable(n.Role, m)
	if enabled, ok := boolField(m, "enabled", "AXEnabled"); ok && !enabled {
		n.Disabled = true
	}
	for _, key := range []string{"children", "AXChildren"} {
		if c, ok := m[key]; ok {
			n.Children = append(n.Children, compactAny(c)...)
		}
	}
	n.Children = dedupeSiblings(pruneDecoration(n))
	if n.isNoise() {
		return n.Children
	}
	return []*Node{n}
}

// pruneDecoration drops the label fragments and icons UIKit nests inside a
// labelled control (a Button whose AXLabel already concatenates its child
// texts), which would otherwise double the snapshot for no information.
func pruneDecoration(parent *Node) []*Node {
	if !parent.Interactable || parent.Label == "" {
		return parent.Children
	}
	kept := parent.Children[:0]
	for _, c := range parent.Children {
		if c.redundantWithin(parent.Label) {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// redundantWithin reports whether a node only restates part of an
// enclosing control's label, or is decoration (an icon) inside it.
func (n *Node) redundantWithin(parentLabel string) bool {
	if n.Interactable || n.Value != "" || len(n.Children) > 0 {
		return false
	}
	return n.Role == "Image" || n.Label == "" || strings.Contains(parentLabel, n.Label)
}

// dedupeSiblings drops repeated identical siblings; UIKit's a11y tree
// exposes some controls (e.g. a search field's dictate button) twice.
func dedupeSiblings(nodes []*Node) []*Node {
	if len(nodes) < 2 {
		return nodes
	}
	seen := make(map[string]bool, len(nodes))
	out := nodes[:0]
	for _, n := range nodes {
		key, err := json.Marshal(n)
		if err == nil {
			if seen[string(key)] {
				continue
			}
			seen[string(key)] = true
		}
		out = append(out, n)
	}
	return out
}

// isNoise reports whether the node carries no information of its own and
// should be replaced by its children.
func (n *Node) isNoise() bool {
	if n.Value != "" || n.Placeholder != "" || n.Interactable {
		return false
	}
	switch n.Role {
	case "", "Other", "Application", "Window", "Group":
	default:
		return false
	}
	if n.Identifier != "" {
		// Identifiers are developer-set lookup keys (wait_for_element `id`
		// predicates), so identified nodes are kept — except the wrapper
		// whose label merely restates its id (UIKit's intermittent Toolbar
		// group), which appears nondeterministically and would make the
		// tree hash flap on an unchanged screen.
		return n.Label == n.Identifier && len(n.Children) > 0
	}
	return n.Label == ""
}

// interactableRoles are element roles an agent can act on directly.
var interactableRoles = map[string]bool{
	"Button": true, "Link": true, "TextField": true, "SecureTextField": true,
	"SearchField": true, "Switch": true, "Slider": true, "Cell": true,
	"Key": true, "TabBarItem": true, "SegmentedControl": true, "Stepper": true,
	"PageIndicator": true, "Picker": true, "PickerWheel": true, "CheckBox": true,
	"RadioButton": true, "MenuItem": true, "TextView": true, "DatePicker": true,
}

func isInteractable(role string, m map[string]any) bool {
	if v, ok := boolField(m, "interactable", "AXInteractable"); ok && v {
		return true
	}
	return interactableRoles[role]
}

// normalizeRole strips the AXCustom/AX prefixes AXe emits for element types
// so roles read as plain names ("Button", "Cell").
func normalizeRole(role string) string {
	role = strings.TrimPrefix(role, "AX")
	if i := strings.LastIndex(role, "."); i >= 0 {
		role = role[i+1:]
	}
	return role
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			if s = strings.TrimSpace(s); s != "" && s != "(null)" {
				return s
			}
		}
		if n, ok := m[k].(json.Number); ok {
			return n.String()
		}
	}
	return ""
}

func boolField(m map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v, true
		}
	}
	return false, false
}

// parseFrame reads an element rect from either the structured `frame`
// object AXe emits or the AXFrame string form "{{x, y}, {w, h}}".
func parseFrame(m map[string]any) *Frame {
	for _, key := range []string{"frame", "AXFrame", "rect"} {
		switch v := m[key].(type) {
		case map[string]any:
			f := &Frame{
				X: numOr(v, 0, "x", "X"),
				Y: numOr(v, 0, "y", "Y"),
				W: numOr(v, 0, "width", "w", "Width"),
				H: numOr(v, 0, "height", "h", "Height"),
			}
			if f.W > 0 || f.H > 0 {
				return f
			}
		case string:
			if f := parseFrameString(v); f != nil {
				return f
			}
		}
	}
	return nil
}

// parseFrameString parses CoreGraphics' "{{x, y}, {w, h}}" description.
func parseFrameString(s string) *Frame {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == '{' || r == '}' || r == ',' || r == ' '
	})
	if len(fields) != 4 {
		return nil
	}
	var vals [4]float64
	for i, f := range fields {
		n, err := json.Number(f).Float64()
		if err != nil {
			return nil
		}
		vals[i] = n
	}
	return &Frame{X: vals[0], Y: vals[1], W: vals[2], H: vals[3]}
}

func numOr(m map[string]any, def float64, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if n, err := toNum(v); err == nil {
				return n
			}
		}
	}
	return def
}

// rawViewport extracts the device viewport from a raw describe-ui
// document: the shallowest origin-anchored frame is the screen bounds.
// The root AXApplication element carries no frame in real AXe output —
// the screen rectangle sits on it or on a wrapper Group a level or two
// below — so the search proceeds breadth-first and stops at the first
// level with a candidate. Taking the shallowest (not the largest) keeps
// an over-tall (0,0)-anchored scroll content container deeper in the
// tree from being mistaken for the screen. Ties among siblings go to the
// largest. No candidate within a few levels yields nil (no viewport
// check).
func rawViewport(raw []byte) *Frame {
	var root any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil
	}
	level := flatMaps([]any{root})
	for depth := 0; depth <= 2 && len(level) > 0; depth++ {
		var best *Frame
		for _, m := range level {
			if f := viewportFrame(parseFrame(m)); f != nil {
				if best == nil || f.W*f.H > best.W*best.H {
					best = f
				}
			}
		}
		if best != nil {
			return best
		}
		var next []any
		for _, m := range level {
			for _, key := range []string{"children", "AXChildren"} {
				if ch, ok := m[key].([]any); ok {
					next = append(next, ch...)
				}
			}
		}
		level = flatMaps(next)
	}
	return nil
}

// flatMaps flattens nested arrays into the maps they contain.
func flatMaps(vs []any) []map[string]any {
	var out []map[string]any
	for _, v := range vs {
		switch t := v.(type) {
		case []any:
			out = append(out, flatMaps(t)...)
		case map[string]any:
			out = append(out, t)
		}
	}
	return out
}

// viewportFrame validates a candidate viewport rectangle: the screen is
// anchored at the origin with positive size.
func viewportFrame(f *Frame) *Frame {
	if f == nil || f.W <= 0 || f.H <= 0 || f.X != 0 || f.Y != 0 {
		return nil
	}
	return f
}

// TreeHash is a stable content hash of a compacted tree, so callers can
// cheaply detect whether the UI changed between observations.
func TreeHash(nodes []*Node) string {
	b, err := json.Marshal(nodes)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:8])
}
