// Package domain is a pure domain model of core-banking, with zero DB/framework dependencies (innermost layer).
package domain

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

var osReadFile = os.ReadFile

// Money represents the amount in int64 cents. Financial ban float.
// Constructed only via NewMoneyFromCents or ParseCents - no float entry.
type Money int64

// NewMoneyFromCents is constructed directly from points (recommended entry, no float).
func NewMoneyFromCents(cents int64) Money { return Money(cents) }

// ParseCents Parses a NUMERIC(18,2) string (such as "1234.56") into cents (123456).
// Pure integer operations, eliminating float precision issues.
func ParseCents(s string) (Money, error) {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > 2 {
		return 0, fmt.Errorf("money: 小数位超过 2: %q", s)
	}
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	n, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: 解析 %q 失败: %w", s, err)
	}
	if neg {
		n = -n
	}
	return Money(n), nil
}

// Add returns m+o.
func (m Money) Add(o Money) Money { return m + o }

// Sub returns m-o.
func (m Money) Sub(o Money) Money { return m - o }

// Cents Returns the points value.
func (m Money) Cents() int64 { return int64(m) }

// String returns a NUMERIC(18,2) style string (for writing to DB), such as "1234.56".
func (m Money) String() string {
	n := int64(m)
	neg := n < 0
	if neg {
		n = -n
	}
	yuan, cents := n/100, n%100
	if neg {
		return fmt.Sprintf("-%d.%02d", yuan, cents)
	}
	return fmt.Sprintf("%d.%02d", yuan, cents)
}
