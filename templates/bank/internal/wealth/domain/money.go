package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// Money represents an amount in int64 cents. Binary fractional arithmetic is prohibited to prevent precision loss. Construct values only with NewMoneyFromCents or ParseCents.
type Money int64

func NewMoneyFromCents(cents int64) Money { return Money(cents) }

// ParseCents parses a NUMERIC(18,2) string into cents using integer-only arithmetic.
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

func (m Money) Add(o Money) Money { return m + o }
func (m Money) Sub(o Money) Money { return m - o }
func (m Money) Cents() int64      { return int64(m) }

// String returns NUMERIC(18,2) style string (for writing to DB).
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
