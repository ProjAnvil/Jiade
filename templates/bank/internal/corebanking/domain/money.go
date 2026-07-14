// Package domain 是 core-banking 的纯领域模型，零 DB/框架依赖（最内层）。
package domain

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

var osReadFile = os.ReadFile

// Money 用 int64 分表示金额。金融禁 float。
// 构造仅经 NewMoneyFromCents 或 ParseCents——无 float 入口。
type Money int64

// NewMoneyFromCents 直接由分构造（推荐入口，无 float）。
func NewMoneyFromCents(cents int64) Money { return Money(cents) }

// ParseCents 把 NUMERIC(18,2) 字符串（如 "1234.56"）解析为分（123456）。
// 纯整数运算，杜绝 float 精度问题。
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

// Add 返回 m+o。
func (m Money) Add(o Money) Money { return m + o }

// Sub 返回 m-o。
func (m Money) Sub(o Money) Money { return m - o }

// Cents 返回分值。
func (m Money) Cents() int64 { return int64(m) }

// String 返回 NUMERIC(18,2) 风格字符串（写入 DB 用），如 "1234.56"。
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
