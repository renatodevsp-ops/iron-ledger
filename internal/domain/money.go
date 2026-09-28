package domain

import (
	"strings"
)

// Currency is an ISO 4217 code. Only the two released currencies are
// representable; there is no conversion between them in this release (D-3).
type Currency string

const (
	BRL Currency = "BRL"
	USD Currency = "USD"
)

// minorUnitDigits is the number of decimal places of every supported currency.
// BRL and USD both have 2, so one minor unit is 0.01 of the major unit.
const minorUnitDigits = 2

// minorUnitScale converts minor units to major units using integer arithmetic
// only. There is deliberately no float divisor anywhere in this package.
const minorUnitScale = int64(100)

// Money is an exact monetary amount: an integer in the currency's minimum unit
// plus the currency code. There is no float32/float64 representation of money
// anywhere in this type (Constitution Principle I).
type Money struct {
	AmountMinor int64
	Currency    Currency
}

// Parse converts a decimal string into Money without ever touching a float and
// without ever rounding. It rejects the empty string, non-numeric input,
// negative values, exponent notation and more decimal places than the currency
// has minor units.
func Parse(s string, c Currency) (Money, error) {
	if err := ValidateCurrency(c); err != nil {
		return Money{}, err
	}
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Money{}, NewError(ReasonInvalidAmount, "amount is empty").WithField("amountMinor")
	}
	if strings.ContainsAny(raw, "eE") {
		return Money{}, NewError(ReasonInvalidAmount, "exponent notation is not accepted: %q", s).WithField("amountMinor")
	}
	sign := int64(1)
	switch raw[0] {
	case '-':
		sign = -1
		raw = raw[1:]
	case '+':
		raw = raw[1:]
	}
	if raw == "" {
		return Money{}, NewError(ReasonInvalidAmount, "amount is empty").WithField("amountMinor")
	}
	whole, frac, hasFrac := strings.Cut(raw, ".")
	if whole == "" {
		whole = "0"
	}
	if !isDigits(whole) || (hasFrac && !isDigits(frac)) {
		return Money{}, NewError(ReasonInvalidAmount, "amount is not a decimal number: %q", s).WithField("amountMinor")
	}
	if hasFrac {
		if len(frac) > minorUnitDigits {
			return Money{}, NewError(ReasonInvalidAmount,
				"amount has more precision than the currency minor unit: %q", s).WithField("amountMinor")
		}
		frac += strings.Repeat("0", minorUnitDigits-len(frac))
	}
	wholeMinor, err := parseInt64(whole)
	if err != nil {
		return Money{}, NewError(ReasonInvalidAmount, "amount is out of range: %q", s).WithField("amountMinor")
	}
	var fracMinor int64
	if hasFrac {
		fracMinor, err = parseInt64(frac)
		if err != nil {
			return Money{}, NewError(ReasonInvalidAmount, "amount is out of range: %q", s).WithField("amountMinor")
		}
	}
	total := wholeMinor*minorUnitScale + fracMinor
	if sign < 0 {
		total = -total
	}
	if total < 0 {
		return Money{}, NewError(ReasonInvalidAmount, "amount must not be negative: %q", s).WithField("amountMinor")
	}
	return Money{AmountMinor: total, Currency: c}, nil
}

// ValidateCurrency rejects any currency outside the released set.
// ValidCurrency reports whether c is one of the two supported currencies.
// There is no implicit conversion between them, so an unsupported code is a
// validation error rather than something to be converted.
func ValidCurrency(c Currency) bool {
	return c == BRL || c == USD
}

func ValidateCurrency(c Currency) error {
	switch c {
	case BRL, USD:
		return nil
	default:
		return NewError(ReasonInvalidCurrency, "unsupported currency %q; supported: BRL, USD", string(c)).WithField("currency")
	}
}

// Add returns a+m. The currencies must be equal: there is no conversion.
func (m Money) Add(other Money) (Money, error) {
	if err := sameCurrency(m, other); err != nil {
		return Money{}, err
	}
	return Money{AmountMinor: m.AmountMinor + other.AmountMinor, Currency: m.Currency}, nil
}

// Sub returns m-other. The currencies must be equal: there is no conversion.
func (m Money) Sub(other Money) (Money, error) {
	if err := sameCurrency(m, other); err != nil {
		return Money{}, err
	}
	return Money{AmountMinor: m.AmountMinor - other.AmountMinor, Currency: m.Currency}, nil
}

func sameCurrency(a, b Money) error {
	if a.Currency != b.Currency {
		return NewError(ReasonCurrencyMismatch, "cannot combine %s with %s", a.Currency, b.Currency).WithField("currency")
	}
	return nil
}

// IsZero reports whether the amount is exactly zero.
func (m Money) IsZero() bool { return m.AmountMinor == 0 }

// IsPositive reports whether the amount is strictly greater than zero, the
// condition every incoming operation amount must satisfy.
func (m Money) IsPositive() bool { return m.AmountMinor > 0 }

// LessThan reports whether m is strictly less than other, same currency only.
func (m Money) LessThan(other Money) bool {
	return m.Currency == other.Currency && m.AmountMinor < other.AmountMinor
}

// String renders the amount for humans using integer arithmetic only: it
// divides by the minor-unit scale with / and %, never with a float.
func (m Money) String() string {
	amount := m.AmountMinor
	sign := ""
	if amount < 0 {
		sign = "-"
		// -math.MinInt64 overflows int64, so negate through uint64.
		amount = -int64(uint64(amount))
	}
	units := amount / minorUnitScale
	frac := amount % minorUnitScale
	var b strings.Builder
	b.WriteString(string(m.Currency))
	b.WriteByte(' ')
	b.WriteString(sign)
	b.WriteString(intToString(units))
	b.WriteByte('.')
	if frac < 10 {
		b.WriteByte('0')
	}
	b.WriteString(intToString(frac))
	return b.String()
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseInt64 is a minimal, allocation-free integer parser. strconv.Atoi is
// deliberately avoided on the money path so the accepted grammar is exactly
// "digits" and nothing else.
func parseInt64(s string) (int64, error) {
	var n int64
	for i := 0; i < len(s); i++ {
		d := int64(s[i] - '0')
		if n > (1<<63-1-d)/10 {
			return 0, errOutOfRange
		}
		n = n*10 + d
	}
	return n, nil
}

func intToString(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -int64(uint64(n))
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
