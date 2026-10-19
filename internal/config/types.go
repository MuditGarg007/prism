package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type ByteSize int64

var byteUnits = map[string]int64{
	"":   1,
	"B":  1,
	"KB": 1 << 10,
	"MB": 1 << 20,
	"GB": 1 << 30,
}

func ParseByteSize(s string) (ByteSize, error) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	if i == -1 {
		i = len(s)
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	mult, ok := byteUnits[strings.ToUpper(strings.TrimSpace(s[i:]))]
	if !ok {
		return 0, fmt.Errorf("invalid size %q: unit must be B, KB, MB or GB", s)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return ByteSize(n * mult), nil
}

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: size must be a scalar", n.Line)
	}
	v, err := ParseByteSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}

// Rate is a number of events per time unit, written as "100/s", "600/m"
// or "5000/h".
type Rate struct {
	Count int
	Per   time.Duration
}

var rateUnits = map[string]time.Duration{
	"s": time.Second,
	"m": time.Minute,
	"h": time.Hour,
}

// ParseRate parses rates such as "100/s".
func ParseRate(s string) (Rate, error) {
	count, unit, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return Rate{}, fmt.Errorf("invalid rate %q: want <count>/<s|m|h>", s)
	}
	n, err := strconv.Atoi(count)
	if err != nil || n <= 0 {
		return Rate{}, fmt.Errorf("invalid rate %q: count must be a positive integer", s)
	}
	per, ok := rateUnits[unit]
	if !ok {
		return Rate{}, fmt.Errorf("invalid rate %q: unit must be s, m or h", s)
	}
	return Rate{Count: n, Per: per}, nil
}

func (r *Rate) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: rate must be a scalar", n.Line)
	}
	v, err := ParseRate(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*r = v
	return nil
}
