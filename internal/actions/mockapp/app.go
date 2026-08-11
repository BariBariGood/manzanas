// Package mockapp is the mock action backend behind `manzanasd --mock`: a
// deterministic synthetic app screen per mock target, so the full
// element/observe/audit action loop runs off-Mac (CI, Linux dev boxes)
// with no simulator. The synthetic UI is a small login screen whose state
// transitions on taps and typing, and whose screenshots are rendered from
// the same element tree that describe-ui reports — what you see matches
// what you observe.
package mockapp

import (
	"encoding/json"
	"strings"
	"sync"
)

// Screen geometry (points). The content is taller than the viewport so
// scroll_to_element has something real to scroll to.
const (
	ScreenW = 390
	ScreenH = 844

	contentH = 1000

	keyboardY = 554
	keyboardH = 290
)

// element is one node of the synthetic UI in content coordinates.
type element struct {
	Role        string
	Label       string
	Value       string
	Placeholder string
	ID          string
	X, Y, W, H  float64
	// overlay elements (the keyboard) are positioned in screen
	// coordinates and do not scroll with the content.
	overlay bool
}

// App is the deterministic synthetic app screen for one mock target: a
// login form (two text fields, a switch, buttons) plus an off-screen
// footer. Taps toggle state, typing edits the focused field, and a swipe
// scrolls, so wait_for_element/scroll_to_element have real transitions to
// wait on. All methods are safe for concurrent use.
type App struct {
	mu       sync.Mutex
	username string
	password string
	wifiOn   bool
	focus    string // "", "username" or "password"
	status   string // status label after a Sign In tap; "" hides it
	scrollY  float64
	launches int
}

// NewApp returns the app in its initial state.
func NewApp() *App { return &App{} }

// Reset restores the initial screen state.
func (a *App) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reset()
}

func (a *App) reset() {
	a.username, a.password, a.status, a.focus = "", "", "", ""
	a.wifiOn = false
	a.scrollY = 0
}

// Launch resets the screen (a fresh launch always shows the initial
// state, keeping mock runs deterministic) and returns a fake pid.
func (a *App) Launch() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reset()
	a.launches++
	return 4200 + a.launches
}

// maxScroll is how far the content can scroll.
func maxScroll() float64 { return contentH - ScreenH }

// elements builds the current content-space element list from the state.
// It is the single source of truth for describe-ui, hit-testing and
// screenshot rendering.
func (a *App) elements() []element {
	els := []element{
		{Role: "StaticText", Label: "Mock Login", ID: "title", X: 20, Y: 80, W: 350, H: 40},
		{Role: "TextField", Placeholder: "Username", Value: a.username, ID: "username", X: 20, Y: 160, W: 350, H: 44},
		{Role: "SecureTextField", Placeholder: "Password", Value: strings.Repeat("*", len([]rune(a.password))), ID: "password", X: 20, Y: 224, W: 350, H: 44},
		{Role: "Switch", Label: "Wi-Fi", Value: boolValue(a.wifiOn), ID: "wifi", X: 20, Y: 300, W: 60, H: 44},
		{Role: "Button", Label: "Sign In", ID: "sign-in", X: 20, Y: 368, W: 350, H: 50},
	}
	if a.status != "" {
		els = append(els, element{Role: "StaticText", Label: a.status, ID: "status", X: 20, Y: 438, W: 350, H: 30})
	}
	els = append(els,
		element{Role: "Button", Label: "Reset", ID: "reset", X: 20, Y: 492, W: 350, H: 44},
		element{Role: "StaticText", Label: "Mock Footer", ID: "footer", X: 20, Y: 940, W: 350, H: 30},
	)
	if a.focus != "" {
		els = append(els, element{Role: "Keyboard", Label: "Keyboard", ID: "keyboard",
			X: 0, Y: keyboardY, W: ScreenW, H: keyboardH, overlay: true})
	}
	return els
}

func boolValue(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// screenFrame maps a content-space element onto screen coordinates,
// applying the scroll offset (overlays are already in screen space).
func (a *App) screenFrame(e element) (x, y, w, h float64) {
	if e.overlay {
		return e.X, e.Y, e.W, e.H
	}
	return e.X, e.Y - a.scrollY, e.W, e.H
}

// Tap hits the topmost element containing the screen point and applies
// its state transition: text fields take focus (bringing up the
// keyboard), the switch toggles, Sign In shows a status label, Reset
// restores the initial state. A tap on nothing dismisses focus.
func (a *App) Tap(x, y float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	els := a.elements()
	for i := len(els) - 1; i >= 0; i-- {
		e := els[i]
		ex, ey, ew, eh := a.screenFrame(e)
		if x < ex || x >= ex+ew || y < ey || y >= ey+eh {
			continue
		}
		switch e.ID {
		case "username", "password":
			a.focus = e.ID
		case "keyboard":
			// Keys land via Type, not tap coordinates.
		case "wifi":
			a.wifiOn = !a.wifiOn
			a.focus = ""
		case "sign-in":
			if a.username != "" && a.password != "" {
				a.status = "Welcome, " + a.username + "!"
			} else {
				a.status = "Missing credentials"
			}
			a.focus = ""
		case "reset":
			a.reset()
		default:
			a.focus = ""
		}
		return
	}
	a.focus = ""
}

// Type appends text to the focused field; with no focus the keystrokes
// land nowhere, exactly like a real simulator with no first responder.
func (a *App) Type(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch a.focus {
	case "username":
		a.username += text
	case "password":
		a.password += text
	}
}

// Swipe scrolls the content vertically by the drag distance (dragging the
// finger up reveals content below), clamped to the scrollable range.
func (a *App) Swipe(x1, y1, x2, y2 float64) {
	_ = x1
	_ = x2
	a.mu.Lock()
	defer a.mu.Unlock()
	a.scrollY += y1 - y2
	if a.scrollY < 0 {
		a.scrollY = 0
	}
	if m := maxScroll(); a.scrollY > m {
		a.scrollY = m
	}
}

// DescribeUI renders the current tree as AXe-style describe-ui JSON: one
// root Application element at the screen bounds with the elements as
// children, frames in screen coordinates.
func (a *App) DescribeUI() ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	children := []map[string]any{}
	for _, e := range a.elements() {
		x, y, w, h := a.screenFrame(e)
		m := map[string]any{
			"role":    e.Role,
			"enabled": true,
			"frame":   map[string]any{"x": x, "y": y, "width": w, "height": h},
		}
		if e.Label != "" {
			m["AXLabel"] = e.Label
		}
		if e.Value != "" {
			m["AXValue"] = e.Value
		}
		if e.Placeholder != "" {
			m["AXPlaceholderValue"] = e.Placeholder
		}
		if e.ID != "" {
			m["AXUniqueId"] = e.ID
		}
		children = append(children, m)
	}
	root := map[string]any{
		"role":     "Application",
		"AXLabel":  "MockApp",
		"frame":    map[string]any{"x": 0, "y": 0, "width": ScreenW, "height": ScreenH},
		"children": children,
	}
	return json.Marshal([]any{root})
}
