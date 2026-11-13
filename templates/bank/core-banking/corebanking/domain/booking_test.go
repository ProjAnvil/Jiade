package domain

import (
	"strings"
	"testing"
)

func TestNewVoucherNo_FormatAndUniqueness(t *testing.T) {
	v1 := NewVoucherNo("2026-07-16")
	if !strings.HasPrefix(v1, "V20260716") || len(v1) != len("V20260716")+16 {
		t.Errorf("voucher wrong format: %q (want V+8-digit date+16 hex)", v1)
	}
	v2 := NewVoucherNo("2026-07-16")
	if v1 == v2 {
		t.Errorf("two generations should differ: %s", v1)
	}
}

func TestBookingConstants(t *testing.T) {
	if CashSubject != "1001" {
		t.Errorf("CashSubject=%q want 1001", CashSubject)
	}
	if ActionDeposit != "deposit" || ActionWithdraw != "withdraw" || ActionTransfer != "transfer" {
		t.Error("Action constants wrong")
	}
	if ReverseBlue != "blue" || ReverseRed != "red" {
		t.Error("ReverseMode constants wrong")
	}
	if TxnStatusNormal != "normal" || TxnStatusReversed != "reversed" {
		t.Error("TxnStatus constants wrong")
	}
}
