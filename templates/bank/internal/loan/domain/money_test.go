package domain

import (
	"os"
	"strings"
	"testing"
)

func TestMoneyRoundTrip(t *testing.T) {
	m := NewMoneyFromCents(123456)
	if m.String() != "1234.56" {
		t.Errorf("String=%s want 1234.56", m.String())
	}
	p, err := ParseCents("1234.56")
	if err != nil || p != m {
		t.Errorf("ParseCents 回环失败: %v %v", p, err)
	}
}

// TestSourceHasNoFloat guards that the money.go source code does not contain float (float is prohibited in finance).
func TestSourceHasNoFloat(t *testing.T) {
	b, err := os.ReadFile("money.go")
	if err != nil {
		t.Fatal("读不到 money.go:", err)
	}
	if strings.Contains(string(b), "float") {
		t.Fatal("money.go 含 float，违反金融不变量")
	}
}
