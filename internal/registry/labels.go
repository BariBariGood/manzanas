package registry

import (
	"regexp"
	"strings"
)

var nonAlnum = regexp.MustCompile(`[^a-z0-9.]+`)

// slugify lowercases and hyphenates a human-readable name:
// "iPhone 17 Pro" -> "iphone-17-pro".
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonAlnum.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

var runtimeVer = regexp.MustCompile(`(?i)ios[ -]?([0-9]+)(?:\.([0-9]+))?`)

// DeriveLabels computes the label set for a target from its runtime and
// device type, e.g. ("iOS 26.5", "iPhone 17 Pro") ->
// ["simulator", "ios26", "ios26.5", "iphone-17-pro"].
func DeriveLabels(kind, runtime, deviceType string) []string {
	labels := []string{kind}
	if m := runtimeVer.FindStringSubmatch(runtime); m != nil {
		labels = append(labels, "ios"+m[1])
		if m[2] != "" {
			labels = append(labels, "ios"+m[1]+"."+m[2])
		}
	}
	if dt := slugify(deviceType); dt != "" {
		labels = append(labels, dt)
	}
	return labels
}

// RequirementLabels translates human-readable runtime / device-type
// requirements into the derived label slugs targets carry, e.g.
// ("iOS 26.5", "iPhone 17 Pro") -> ["ios26.5", "iphone-17-pro"]. A
// runtime without a minor version yields the major slug ("iOS 26" ->
// "ios26"). Used by the run primitive's target matching.
func RequirementLabels(runtime, deviceType string) []string {
	var labels []string
	if m := runtimeVer.FindStringSubmatch(runtime); m != nil {
		l := "ios" + m[1]
		if m[2] != "" {
			l += "." + m[2]
		}
		labels = append(labels, l)
	} else if rt := slugify(runtime); rt != "" {
		labels = append(labels, rt)
	}
	if dt := slugify(deviceType); dt != "" {
		labels = append(labels, dt)
	}
	return labels
}
