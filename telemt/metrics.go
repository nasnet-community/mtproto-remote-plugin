package telemt

import (
	"bufio"
	"strconv"
	"strings"
)

// ParseMetric reads one metric family out of telemt's Prometheus text output and
// returns the summed value per `user` label. Comments, other metrics and samples
// without a user label are ignored; a longer metric name sharing the prefix does
// not match (the sample must be exactly `name{`).
func ParseMetric(text, name string) map[string]uint64 {
	out := make(map[string]uint64)
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name) {
			continue
		}
		rest := line[len(name):]
		if !strings.HasPrefix(rest, "{") {
			continue
		}
		end := labelsEnd(rest)
		if end < 0 {
			continue
		}
		user, ok := labelValue(rest[1:end], "user")
		if !ok {
			continue
		}
		val := strings.TrimSpace(rest[end+1:])
		if sp := strings.IndexByte(val, ' '); sp >= 0 {
			val = val[:sp] // an optional trailing timestamp
		}
		f, err := strconv.ParseFloat(val, 64)
		if err != nil || f < 0 {
			continue
		}
		out[user] += uint64(f)
	}
	return out
}

// labelsEnd finds the closing brace, skipping braces inside quoted values.
func labelsEnd(s string) int {
	inQuote := false
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && inQuote:
			i++
		case c == '"':
			inQuote = !inQuote
		case c == '}' && !inQuote:
			return i
		}
	}
	return -1
}

// labelValue extracts key from a label set like `user="a@x",mode="tls"`,
// handling escaped quotes and backslashes in values.
func labelValue(labels, key string) (string, bool) {
	i := 0
	for i < len(labels) {
		eq := strings.IndexByte(labels[i:], '=')
		if eq < 0 {
			return "", false
		}
		k := strings.TrimSpace(labels[i : i+eq])
		j := i + eq + 1
		if j >= len(labels) || labels[j] != '"' {
			return "", false
		}
		j++
		var sb strings.Builder
		for j < len(labels) {
			ch := labels[j]
			if ch == '\\' && j+1 < len(labels) {
				sb.WriteByte(labels[j+1])
				j += 2
				continue
			}
			if ch == '"' {
				break
			}
			sb.WriteByte(ch)
			j++
		}
		if k == key {
			return sb.String(), true
		}
		j++
		if j < len(labels) && labels[j] == ',' {
			j++
		}
		i = j
	}
	return "", false
}

// Metric names telemt exposes for per-user traffic.
const (
	MetricUp   = "telemt_user_octets_from_client"
	MetricDown = "telemt_user_octets_to_client"
)
