package monthlyusage

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
)

var (
	countPattern = regexp.MustCompile(`^[0-9]{1,308}$`)
	costPattern  = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]{1,5})?$`)
)

type totals struct {
	// Requests, then input, output, cache read and total tokens, all as supplied by SQL.
	counts [5]decimal.Decimal
	cost   decimal.Decimal
}

func parseTotals(row Row) (totals, error) {
	var t totals
	for i, raw := range [...]string{row.Requests, row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.TotalTokens} {
		if !countPattern.MatchString(raw) {
			return t, fmt.Errorf("invalid count in %s", metricHeaders[i])
		}
		t.counts[i], _ = decimal.NewFromString(raw) // validated unsigned integer
	}
	if len(row.ActualCost) > maxText || !costPattern.MatchString(row.ActualCost) {
		return t, fmt.Errorf("invalid decimal in actual_cost")
	}
	t.cost, _ = decimal.NewFromString(row.ActualCost) // exponent is at most five digits
	if exp := t.cost.Exponent(); exp < -maxText || exp > maxText {
		return t, fmt.Errorf("actual_cost scale exceeds Excel cell limit")
	}
	return t, nil
}

func (t *totals) add(other totals) {
	for i := range t.counts {
		t.counts[i] = t.counts[i].Add(other.counts[i])
	}
	t.cost = t.cost.Add(other.cost)
}

type number struct {
	value        decimal.Decimal
	style        int
	negativeZero bool // Decimal discards this sign; Python preserves it in the detail cell.
}

const (
	integerStyle    = 2
	costStyle       = 3
	tokenStyle      = 4
	smallTokenStyle = 5
	scientificStyle = 7
)

func integer(n int64) number {
	return number{value: decimal.NewFromInt(n), style: integerStyle}
}

// shift is zero for detail, -6 for users and -9 for departments.
func (t totals) numbers(prefix []number, shift int32, negativeZero bool) []number {
	for i, d := range t.counts {
		n := number{value: d, style: integerStyle}
		if i > 0 && shift != 0 {
			n = number{value: d.Shift(shift), style: tokenStyle}
		}
		prefix = append(prefix, n)
	}
	if shift != 0 {
		return append(prefix, number{value: decimal.NewFromBigInt(t.cost.BigInt(), 0), style: integerStyle})
	}
	return append(prefix, number{value: t.cost, style: costStyle, negativeZero: negativeZero})
}

// fixedPoint preserves the input scale, including trailing zeros.
func fixedPoint(digits string, exp int) string {
	if exp >= 0 {
		if digits == "0" {
			return digits
		}
		return digits + strings.Repeat("0", exp)
	}
	point := len(digits) + exp
	if point <= 0 {
		return "0." + strings.Repeat("0", -point) + digits
	}
	return digits[:point] + "." + digits[point:]
}

// checkMagnitude compares normalized decimal significands at the two Excel endpoints.
func checkMagnitude(significant string, adjusted int) error {
	if significant != "" && (adjusted < -308 || adjusted == -308 && significant < "22250738585072014" ||
		adjusted > 307 || adjusted == 307 && significant > "999999999999999") {
		return fmt.Errorf("number outside Excel's supported range")
	}
	return nil
}

type renderedNumber struct {
	value string
	style int // zero means exact text
	width int
}

func (n number) render() (renderedNumber, error) {
	abs := n.value.Abs()
	digits := abs.Coefficient().String()
	exp := int(abs.Exponent())
	adjusted := len(digits) - 1 + exp
	significant := strings.TrimRight(digits, "0")
	if err := checkMagnitude(significant, adjusted); err != nil {
		return renderedNumber{}, err
	}
	sign := ""
	if n.value.IsNegative() || n.negativeZero {
		sign = "-"
	}
	if len(significant) > 15 {
		return renderedNumber{value: sign + fixedPoint(digits, exp)}, nil
	}

	// Reproduce Decimal.__str__ for numeric XML, also for zeros such as 0E-9.
	var value string
	if n.style != integerStyle && (exp > 0 || adjusted < -6) {
		mantissa := digits
		if len(digits) > 1 {
			mantissa = digits[:1] + "." + digits[1:]
		}
		value = fmt.Sprintf("%s%sE%+d", sign, mantissa, adjusted)
	} else {
		value = sign + fixedPoint(digits, exp)
	}

	style := n.style
	displayed := digits // absolute value; grouping separators are counted below
	switch style {
	case costStyle:
		whole, frac, _ := strings.Cut(abs.StringFixedBank(10), ".")
		frac = strings.TrimRight(frac, "0")
		displayed = whole + "." + frac + strings.Repeat("0", max(0, 2-len(frac)))
	case tokenStyle:
		switch {
		case significant == "":
			displayed = "0"
		case adjusted < -3:
			style = smallTokenStyle
			displayed = strings.TrimRight(abs.StringFixedBank(9), "0")
		default:
			displayed = abs.StringFixedBank(3)
		}
	}
	whole, _, _ := strings.Cut(displayed, ".")
	width := len(sign) + len(displayed) + (len(whole)-1)/3
	if width > 28 {
		// Excel's built-in scientific format uses two fractional digits. Rounding
		// 9.999... to 10.00 can increase the displayed exponent by one.
		if abs.Shift(-int32(adjusted)).RoundBank(2).Equal(decimal.NewFromInt(10)) {
			adjusted++
		}
		style = scientificStyle
		width = len(fmt.Sprintf("%s0.00E%+d", sign, adjusted))
	}
	return renderedNumber{value: value, style: style, width: width}, nil
}
