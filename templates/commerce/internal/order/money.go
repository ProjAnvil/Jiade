package order

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
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

// AllocatePercentageDiscount applies basisPoints to the aggregate and assigns
// rounding cents by descending remainder, then stable line index.
func AllocatePercentageDiscount(amounts []int64, basisPoints int64) ([]int64, error) {
	if len(amounts) == 0 || basisPoints < 0 || basisPoints > 10000 {
		return nil, ErrInvalidMoney
	}
	type remainder struct {
		index int
		value int64
	}
	allocations := make([]int64, len(amounts))
	remainders := make([]remainder, len(amounts))
	var total, allocated int64
	for index, amount := range amounts {
		if amount < 0 {
			return nil, ErrInvalidMoney
		}
		var ok bool
		total, ok = checkedAdd(total, amount)
		if !ok || (amount != 0 && basisPoints > math.MaxInt64/amount) {
			return nil, ErrInvalidMoney
		}
		product := amount * basisPoints
		allocations[index] = product / 10000
		allocated, ok = checkedAdd(allocated, allocations[index])
		if !ok {
			return nil, ErrInvalidMoney
		}
		remainders[index] = remainder{index: index, value: product % 10000}
	}
	if total != 0 && basisPoints > math.MaxInt64/total {
		return nil, ErrInvalidMoney
	}
	target := (total * basisPoints) / 10000
	sort.SliceStable(remainders, func(left, right int) bool {
		return remainders[left].value > remainders[right].value
	})
	for extra := target - allocated; extra > 0; extra-- {
		allocations[remainders[int(extra-1)%len(remainders)].index]++
	}
	return allocations, nil
}

// RegionalTax returns a deterministic checkout tax rate and rounded-down tax
// in minor units. The compact policy is intentionally explicit until a tax
// service owns these jurisdiction rules.
func RegionalTax(country, region string, taxableMinor int64) (int64, int64, error) {
	if taxableMinor < 0 {
		return 0, 0, ErrInvalidMoney
	}
	country = strings.ToUpper(strings.TrimSpace(country))
	region = strings.ToUpper(strings.TrimSpace(region))
	var basisPoints int64
	switch country {
	case "CN":
		basisPoints = 1300
	case "US":
		switch region {
		case "OR", "DE", "MT", "NH":
			basisPoints = 0
		default:
			basisPoints = 725
		}
	case "GB":
		basisPoints = 2000
	case "":
		return 0, 0, ErrInvalidMoney
	default:
		basisPoints = 1000
	}
	if taxableMinor != 0 && basisPoints > math.MaxInt64/taxableMinor {
		return 0, 0, ErrInvalidMoney
	}
	return basisPoints, taxableMinor * basisPoints / 10000, nil
}
