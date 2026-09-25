package plan

import (
	"sort"
	"strings"
)

// MergeProperties merges keys into a Java properties file, preserving the
// order and comments of existing lines. Keys in overrides replace existing
// values in place; new keys are appended in sorted order. It handles the
// subset of the format Minecraft writes: "key=value" lines and "#" comments.
func MergeProperties(existing string, overrides map[string]string) string {
	seen := map[string]bool{}
	var out []string
	lines := strings.Split(strings.TrimRight(existing, "\n"), "\n")
	if existing == "" {
		lines = nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "!") {
			out = append(out, line)
			continue
		}
		key, _, ok := splitProperty(trimmed)
		if !ok {
			out = append(out, line)
			continue
		}
		if v, has := overrides[key]; has {
			out = append(out, key+"="+v)
			seen[key] = true
			continue
		}
		out = append(out, line)
	}
	var missing []string
	for k := range overrides {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	for _, k := range missing {
		out = append(out, k+"="+overrides[k])
	}
	return strings.Join(out, "\n") + "\n"
}

// ParseProperties returns the key/value pairs of a properties file.
func ParseProperties(content string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "!") {
			continue
		}
		if k, v, ok := splitProperty(trimmed); ok {
			props[k] = v
		}
	}
	return props
}

func splitProperty(line string) (key, value string, ok bool) {
	idx := strings.IndexAny(line, "=:")
	if idx < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}
