package eval

import "strings"

// findElement recursively searches an observe tree (decoded JSON: a node
// map or a list of node maps) for a node matching the query. Label and
// Value match as substrings, Role matches exactly.
func findElement(tree any, q *ElementQuery) bool {
	switch node := tree.(type) {
	case []any:
		for _, child := range node {
			if findElement(child, q) {
				return true
			}
		}
	case map[string]any:
		if nodeMatches(node, q) {
			return true
		}
		if children, ok := node["children"]; ok {
			return findElement(children, q)
		}
	}
	return false
}

func nodeMatches(node map[string]any, q *ElementQuery) bool {
	if q.Role != "" {
		if s, _ := node["role"].(string); s != q.Role {
			return false
		}
	}
	if q.Label != "" {
		if s, _ := node["label"].(string); !strings.Contains(s, q.Label) {
			return false
		}
	}
	if q.Value != "" {
		if s, _ := node["value"].(string); !strings.Contains(s, q.Value) {
			return false
		}
	}
	return true
}
