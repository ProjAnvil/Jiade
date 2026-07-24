package order

import (
	"errors"
	"fmt"
	"math"
)

var ErrInvalidMoney = errors.New("invalid order money")

// Line stores monetary values in integer minor units. DiscountMinor is the
// already allocated discount for the entire line.
type Line struct {
	Quantity       int64
	UnitPriceMinor int64
	DiscountMinor  int64
}

type Totals struct {
	Subtotal int64
	Discount int64
	Shipping int64
	Tax      int64
	Total    int64
}

// CalculateTotals performs exact checked integer arithmetic and enforces the
// order equation: total = subtotal - discount + shipping + tax.
func CalculateTotals(lines []Line, shipping, tax int64) (Totals, error) {
	if len(lines) == 0 {
		return Totals{}, fmt.Errorf("%w: at least one line is required", ErrInvalidMoney)
	}
	if shipping < 0 || tax < 0 {
		return Totals{}, fmt.Errorf("%w: shipping and tax cannot be negative", ErrInvalidMoney)
	}

	var subtotal, discount int64
	for index, line := range lines {
		if line.Quantity <= 0 || line.UnitPriceMinor < 0 || line.DiscountMinor < 0 {
			return Totals{}, fmt.Errorf("%w: invalid line %d", ErrInvalidMoney, index)
		}
		gross, ok := checkedMultiply(line.Quantity, line.UnitPriceMinor)
		if !ok || line.DiscountMinor > gross {
			return Totals{}, fmt.Errorf("%w: invalid line %d total", ErrInvalidMoney, index)
		}
		subtotal, ok = checkedAdd(subtotal, gross)
		if !ok {
			return Totals{}, fmt.Errorf("%w: subtotal overflow", ErrInvalidMoney)
		}
		discount, ok = checkedAdd(discount, line.DiscountMinor)
		if !ok {
			return Totals{}, fmt.Errorf("%w: discount overflow", ErrInvalidMoney)
		}
	}
	if discount > subtotal {
		return Totals{}, fmt.Errorf("%w: discount exceeds subtotal", ErrInvalidMoney)
	}
	total := subtotal - discount
	var ok bool
	total, ok = checkedAdd(total, shipping)
	if !ok {
		return Totals{}, fmt.Errorf("%w: total overflow", ErrInvalidMoney)
	}
	total, ok = checkedAdd(total, tax)
	if !ok {
		return Totals{}, fmt.Errorf("%w: total overflow", ErrInvalidMoney)
	}
	return Totals{
		Subtotal: subtotal,
		Discount: discount,
		Shipping: shipping,
		Tax:      tax,
		Total:    total,
	}, nil
}

func checkedMultiply(left, right int64) (int64, bool) {
	if left < 0 || right < 0 {
		return 0, false
	}
	if left != 0 && right > math.MaxInt64/left {
		return 0, false
	}
	return left * right, true
}

func checkedAdd(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || right > math.MaxInt64-left {
		return 0, false
	}
	return left + right, true
}
