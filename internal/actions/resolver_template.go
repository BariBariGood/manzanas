package actions

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"strings"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/internal/imgutil"
	"github.com/BariBariGood/manzanas/internal/templates"
)

// templateResolver is rung 2 of the element ladder: when the AX (or
// OCR) rung misses, a saved template whose name matches the element
// predicate is matched against a fresh capture, so a tap_element can
// land on a control the tree cannot see (a canvas button, a hostile
// app with a dead snapshot source). Templates are looked up under the
// launched bundle (then the _global bundle) for the capture's device
// class; the rung registers only on backends with a template library.
type templateResolver struct {
	store   *templates.Store
	capture func(udid string) frameCaptureFunc
	space   func(udid string) tapSpaceFunc
	bundle  func(udid string) string
	// classMu guards lastClass.
	classMu sync.Mutex
	// lastClass remembers each UDID's device class ("<backend>/<img_w>")
	// from the previous capture, making the pre-capture existence probe
	// class-aware after the first poll so a template stored only for a
	// different class stops costing one screenshot per poll.
	lastClass map[string]string
}

const (
	resolverTemplate   = "template"
	templateRungBudget = 2 * time.Second
)

func (r *templateResolver) name() string          { return resolverTemplate }
func (r *templateResolver) budget() time.Duration { return templateRungBudget }

func (r *templateResolver) resolve(ctx context.Context, udid string, m elementMatcher, refresh bool) (*Node, observation, error) {
	names := templateNameCandidates(m)
	if len(names) == 0 {
		return nil, observation{}, errRungCannotServe
	}
	// Existence pre-check before paying for a capture: an ordinary
	// tap_element whose name keys no template must not cost a
	// screenshot per poll — nor keep a doomed ladder polling. Once a
	// capture has revealed the device class, the probe is class-scoped,
	// so a template stored only for another backend/width is treated
	// as absent from the second poll on.
	known := r.knownClass(udid)
	stored := false
	for _, bundle := range r.bundleCandidates(udid) {
		for _, name := range names {
			if known != "" {
				if r.store.Has(bundle, name, known) {
					stored = true
				}
			} else if r.store.HasNamed(bundle, name) {
				stored = true
			}
		}
	}
	if !stored {
		return nil, observation{}, errRungCannotServe
	}
	frame, backend, err := r.capture(udid)(ctx)
	if err != nil {
		return nil, observation{}, err
	}
	gray := imgutil.GrayDownscale(frame, 0)
	imgW := gray.Bounds().Dx()
	class := fmt.Sprintf("%s/%d", backend, imgW)
	r.noteClass(udid, class)
	served := false
	for _, bundle := range r.bundleCandidates(udid) {
		for _, name := range names {
			raw, _, err := r.store.Get(bundle, name, class)
			if err != nil {
				continue
			}
			img, _, err := image.Decode(bytes.NewReader(raw))
			if err != nil {
				continue
			}
			served = true
			tmpl := imgutil.GrayDownscale(img, 0)
			x, y, score := imgutil.MatchNCC(gray, tmpl)
			if score < defaultTemplateThreshold {
				continue
			}
			scale, err := r.space(udid)(ctx, imgW)
			if err != nil {
				// Capture pixels cannot be mapped onto tap coordinates: a
				// mis-placed tap is worse than failing, and surfacing the
				// hard error keeps the ladder's fail-fast path intact
				// instead of masking it as a transient miss.
				return nil, observation{}, err
			}
			hit := &Node{
				Role: "Template", Label: name, Interactable: true,
				Frame: &Frame{
					X: float64(x) / scale, Y: float64(y) / scale,
					W: float64(tmpl.Bounds().Dx()) / scale, H: float64(tmpl.Bounds().Dy()) / scale,
				},
			}
			if pr, ok := m.(predicate); ok && pr.InFrame != nil {
				// Honour the matcher's in_frame constraint: a pixel match
				// must never land where the caller was excluded.
				cx, cy := hit.Frame.Center()
				if cx < pr.InFrame.X || cx > pr.InFrame.X+pr.InFrame.W ||
					cy < pr.InFrame.Y || cy > pr.InFrame.Y+pr.InFrame.H {
					continue
				}
			}
			_ = r.store.RecordScore(bundle, name, class, score)
			return hit, observation{nodes: []*Node{hit}}, nil
		}
	}
	if !served {
		// Templates exist for the name but none for this capture's
		// device class (a different backend or width): the rung can
		// never serve this resolve.
		return nil, observation{}, errRungCannotServe
	}
	// The screen WAS observed (a capture succeeded, nothing matched):
	// report an empty observation so the ladder escalates with a
	// timeout instead of surfacing another rung's source failure.
	return nil, observation{nodes: []*Node{}}, errNoMatch
}

// knownClass returns the device class seen on this UDID's previous
// capture ("" before any capture succeeded).
func (r *templateResolver) knownClass(udid string) string {
	r.classMu.Lock()
	defer r.classMu.Unlock()
	return r.lastClass[udid]
}

func (r *templateResolver) noteClass(udid, class string) {
	r.classMu.Lock()
	if r.lastClass == nil {
		r.lastClass = map[string]string{}
	}
	r.lastClass[udid] = class
	r.classMu.Unlock()
}

// bundleCandidates orders the store bundles to search: the launched
// bundle first (templates saved for the app under test), then the
// _global bundle save_template defaults to.
func (r *templateResolver) bundleCandidates(udid string) []string {
	if b := r.bundle(udid); b != "" && b != defaultTemplateBundle {
		return []string{b, defaultTemplateBundle}
	}
	return []string{defaultTemplateBundle}
}

// templateNameCandidates derives store lookup names from an element
// matcher: the flat matcher's id and label, as-is when already a valid
// template name, plus a slugged form ("Sign In" → "sign-in") so labels
// with spaces still key a template. Structured DSL predicates carry no
// single name and never hit the template rung; neither do flat matchers
// narrowed by constraints pixels cannot evaluate (role, value,
// placeholder), so a template match never lands where the caller was
// excluded.
func templateNameCandidates(m elementMatcher) []string {
	pr, ok := m.(predicate)
	if !ok {
		return nil
	}
	if pr.Role != "" || pr.Value != "" || pr.Placeholder != "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] && templates.ValidateName(s) == nil {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range []string{pr.ID, pr.Label} {
		add(s)
		add(templateSlug(s))
	}
	return out
}

// templateSlug maps arbitrary matcher text onto the template store's
// name alphabet: lowercase, with runs of disallowed characters
// collapsed to a single "-".
func templateSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
