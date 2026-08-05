package actions

import (
	"encoding/xml"
	"strconv"
	"strings"
)

func parseXMLNum(s string) (float64, error) { return strconv.ParseFloat(strings.TrimSpace(s), 64) }

// wdaXMLElement mirrors one element of WebDriverAgent's GET /source XML
// (the XCUITest element tree): attributes carry the a11y fields, children
// nest as sub-elements.
type wdaXMLElement struct {
	XMLName  xml.Name
	Name     string          `xml:"name,attr"`
	Label    string          `xml:"label,attr"`
	Value    string          `xml:"value,attr"`
	Enabled  string          `xml:"enabled,attr"`
	X        string          `xml:"x,attr"`
	Y        string          `xml:"y,attr"`
	Width    string          `xml:"width,attr"`
	Height   string          `xml:"height,attr"`
	Children []wdaXMLElement `xml:",any"`
}

// CompactWDATree parses WebDriverAgent XCUITest source XML into the same
// compacted Node tree simulator observe returns, so device and simulator
// observations (and the composite element actions built on them) share
// one shape.
func CompactWDATree(src string) ([]*Node, error) {
	var root wdaXMLElement
	if err := xml.Unmarshal([]byte(src), &root); err != nil {
		return nil, internal("WDA source is not valid XML: %v", err)
	}
	return dedupeSiblings(compactWDAElement(root)), nil
}

// compactWDAElement converts one XCUITest element, collapsing it into its
// children when the element itself carries no information (mirroring
// compactObject for describe-ui JSON).
func compactWDAElement(e wdaXMLElement) []*Node {
	n := &Node{
		Role:  normalizeRole(strings.TrimPrefix(e.XMLName.Local, "XCUIElementType")),
		Label: cleanAttr(e.Label),
		Value: cleanAttr(e.Value),
		Frame: wdaFrame(e),
	}
	// XCUITest's "name" is the accessibility identifier when set by the
	// developer; WDA also mirrors the label into it, which would double
	// every node, so a name that restates the label is dropped.
	if id := cleanAttr(e.Name); id != "" && id != n.Label {
		n.Identifier = id
	}
	n.Interactable = interactableRoles[n.Role]
	if e.Enabled == "false" {
		n.Disabled = true
	}
	for _, c := range e.Children {
		n.Children = append(n.Children, compactWDAElement(c)...)
	}
	n.Children = dedupeSiblings(pruneDecoration(n))
	if n.isNoise() {
		return n.Children
	}
	return []*Node{n}
}

// wdaFrame reads the element rect from the x/y/width/height attributes.
func wdaFrame(e wdaXMLElement) *Frame {
	vals := [4]float64{}
	for i, s := range []string{e.X, e.Y, e.Width, e.Height} {
		if s == "" {
			return nil
		}
		n, err := parseXMLNum(s)
		if err != nil {
			return nil
		}
		vals[i] = n
	}
	f := &Frame{X: vals[0], Y: vals[1], W: vals[2], H: vals[3]}
	if f.W <= 0 && f.H <= 0 {
		return nil
	}
	return f
}

func cleanAttr(s string) string {
	s = strings.TrimSpace(s)
	if s == "(null)" {
		return ""
	}
	return s
}
