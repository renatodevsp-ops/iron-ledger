// Package money implements the Money value object used across every bounded
// context of the ledger.
//
// # Representation
//
// A Money is an amount of a currency held in the currency's minor units (cents
// for BRL, USD, EUR) inside an int64. No value of type float32/float64 ever
// touches an amount: parsing goes string -> int64, arithmetic goes int64 ->
// int64 and persistence stores int64 in a BIGINT column.
//
// Because the scale is fixed at 2 fractional digits for every currency we
// accept, the usable range is
//
//	[-92233720368547758.07, 92233720368547758.07]
//
// Every operation that could leave that range reports ErrOverflow instead of
// wrapping around.
package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Errors returned by parsing and arithmetic. They are sentinel values so
// callers classify with errors.Is.
var (
	// ErrInvalidAmount is returned when a decimal string is not a canonical,
	// fixed-scale monetary amount.
	ErrInvalidAmount = errors.New("money: invalid amount")
	// ErrInvalidCurrency is returned when a currency is not a known ISO 4217
	// alphabetic code.
	ErrInvalidCurrency = errors.New("money: invalid currency")
	// ErrCurrencyMismatch is returned when an operation mixes two different
	// currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow is returned when an operation would leave the int64 range.
	ErrOverflow = errors.New("money: amount overflow")
)

// Scale is the number of fractional digits every amount carries.
const Scale = 2

// maxMajorDigits is the count of integer digits that still fits in an int64
// once Scale fractional digits are added: 92233720368547758 has 17 digits.
const maxMajorDigits = 17

// Currency is an ISO 4217 alphabetic currency code.
type Currency string

// Supported returns the currency codes this service accepts.
func Supported() []string {
	out := make([]string, 0, len(codes))
	for c := range codes {
		out = append(out, string(c))
	}
	return out
}

// codes is the set of ISO 4217 alphabetic codes accepted on the wire. It is a
// curated subset covering the currencies this platform settles in; an unknown
// but well-formed code is still rejected so that a typo cannot create a
// second, meaningless balance.
var codes = map[Currency]struct{}{
	"AED": {}, "ARS": {}, "AUD": {}, "BGN": {}, "BRL": {}, "CAD": {}, "CHF": {},
	"CLP": {}, "CNY": {}, "COP": {}, "CZK": {}, "DKK": {}, "DOP": {}, "EGP": {},
	"EUR": {}, "GBP": {}, "GHS": {}, "HKD": {}, "HUF": {}, "IDR": {}, "ILS": {},
	"INR": {}, "JPY": {}, "KES": {}, "KRW": {}, "MAD": {}, "MXN": {}, "MYR": {},
	"NGN": {}, "NOK": {}, "NZD": {}, "PEN": {}, "PHP": {}, "PLN": {}, "PYG": {},
	"QAR": {}, "RON": {}, "RSD": {}, "RUB": {}, "SAR": {}, "SEK": {}, "SGD": {},
	"THB": {}, "TRY": {}, "TWD": {}, "UAH": {}, "USD": {}, "UYU": {}, "VND": {},
	"ZAR": {},
}

// ParseCurrency validates a currency code.
func ParseCurrency(s string) (Currency, error) {
	if s == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidCurrency)
	}
	if s != strings.ToUpper(s) {
		return "", fmt.Errorf("%w: %q must be uppercase", ErrInvalidCurrency, s)
	}
	c := Currency(s)
	if _, ok := codes[c]; !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidCurrency, s)
	}
	return c, nil
}

// Money is an immutable amount in a single currency.
type Money struct {
	minor    int64
	currency Currency
}

// Zero returns the additive identity for cur.
func Zero(cur Currency) Money { return Money{currency: cur} }

// FromMinor builds a Money from a raw count of minor units. It performs no
// range check: an int64 is by construction inside the representable range.
func FromMinor(minor int64, cur Currency) Money { return Money{minor: minor, currency: cur} }

// MustParse is Parse for package-level constants; it panics on invalid input,
// which can only happen through a programming error.
func MustParse(amount string, cur Currency) Money {
	m, err := Parse(amount, cur)
	if err != nil {
		panic(err)
	}
	return m
}

// Parse builds a Money from a decimal string.
//
// The accepted grammar is exactly
//
//	-? digits{1,17} ( "." digits{1,2} )?
//
// Anything else is rejected rather than rounded: empty strings, "NaN",
// "Infinity", "1e5", "1E+5", "+1.00", "1.234", "1." and " 1.00" all return
// ErrInvalidAmount. Accepted equivalent forms are normalised to two
// fractional digits ("1", "1.5" and "1.50" all parse to 1.50); that
// normalisation happens here, before any hashing, so "1.5" and "1.50" are
// the same request.
func Parse(amount string, cur Currency) (Money, error) {
	currency, err := ParseCurrency(string(cur))
	if err != nil {
		return Money{}, err
	}
	if amount == "" {
		return Money{}, fmt.Errorf("%w: empty amount", ErrInvalidAmount)
	}
	if strings.TrimSpace(amount) != amount {
		return Money{}, fmt.Errorf("%w: %q has surrounding whitespace", ErrInvalidAmount, amount)
	}

	body := amount
	negative := false
	if strings.HasPrefix(body, "-") {
		negative = true
		body = body[1:]
	}

	major, frac, hasFrac := strings.Cut(body, ".")
	if major == "" || !allDigits(major) {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidAmount, amount)
	}
	if len(major) > maxMajorDigits {
		return Money{}, fmt.Errorf("%w: %q exceeds %d integer digits", ErrInvalidAmount, amount, maxMajorDigits)
	}
	if hasFrac {
		if !allDigits(frac) || frac == "" || len(frac) > Scale {
			return Money{}, fmt.Errorf("%w: %q must carry at most %d fractional digits", ErrInvalidAmount, amount, Scale)
		}
	}

	// "1" -> "100"; "1.5" -> "150"; "1.05" -> "105".
	padded := frac + strings.Repeat("0", Scale-len(frac))
	minor, err := strconv.ParseInt(major+padded, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q exceeds int64 range", ErrInvalidAmount, amount)
	}
	if negative {
		minor = -minor
	}
	return Money{minor: minor, currency: currency}, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// AmountMinor returns the raw count of minor units.
func (m Money) AmountMinor() int64 { return m.minor }

// Currency returns the ISO 4217 code the amount is denominated in.
func (m Money) Currency() Currency { return m.currency }

// String renders the amount with exactly Scale fractional digits, e.g.
// "25.00". It is the canonical wire format.
func (m Money) String() string {
	return format(m.minor)
}

func format(minor int64) string {
	negative := minor < 0
	// Negating math.MinInt64 overflows; work on the unsigned magnitude.
	var digits uint64
	if negative {
		digits = uint64(-(minor + 1)) + 1
	} else {
		digits = uint64(minor)
	}
	major := digits / 100
	frac := digits % 100
	out := strconv.FormatUint(major, 10) + "." + pad2(frac)
	if negative {
		return "-" + out
	}
	return out
}

func pad2(v uint64) string {
	s := strconv.FormatUint(v, 10)
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

// IsZero reports whether the amount is exactly zero.
func (m Money) IsZero() bool { return m.minor == 0 }

// IsPositive reports whether the amount is strictly greater than zero.
func (m Money) IsPositive() bool { return m.minor > 0 }

// IsNegative reports whether the amount is strictly less than zero.
func (m Money) IsNegative() bool { return m.minor < 0 }

// Add returns m+o. The currencies must match.
func (m Money) Add(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, err
	}
	sum := m.minor + o.minor
	// Overflow happened iff both operands share a sign that the result does not.
	if (m.minor > 0 && o.minor > 0 && sum < 0) || (m.minor < 0 && o.minor < 0 && sum >= 0) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, o)
	}
	return Money{minor: sum, currency: m.currency}, nil
}

// Sub returns m-o. The currencies must match.
func (m Money) Sub(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, err
	}
	if (m.minor > 0 && o.minor < 0 && m.minor > o.minor) ||
		(m.minor < 0 && o.minor > 0 && m.minor < o.minor) {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrOverflow, m, o)
	}
	return Money{minor: m.minor - o.minor, currency: m.currency}, nil
}

// Neg returns -m. Negating the most negative int64 overflows and is reported.
func (m Money) Neg() (Money, error) {
	if m.minor == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: negating %s", ErrOverflow, m)
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

// Abs returns |m|.
func (m Money) Abs() Money {
	if m.minor < 0 {
		return Money{minor: -m.minor, currency: m.currency}
	}
	return m
}

// Cmp compares two amounts, returning -1, 0 or 1. The currencies must match.
func (m Money) Cmp(o Money) (int, error) {
	if err := m.sameCurrency(o); err != nil {
		return 0, err
	}
	switch {
	case m.minor < o.minor:
		return -1, nil
	case m.minor > o.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal reports whether two amounts denote the same value in the same
// currency. It is false for mismatched currencies, which are never equal.
func (m Money) Equal(o Money) bool { return m.currency == o.currency && m.minor == o.minor }

// Valid reports whether the value is usable: a known currency. The zero Money
// has no currency and is not valid.
func (m Money) Valid() bool {
	if _, ok := codes[m.currency]; !ok {
		return false
	}
	return true
}

func (m Money) sameCurrency(o Money) error {
	if m.currency != o.currency {
		return fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return nil
}

// MarshalJSON renders the wire shape {"amount":"25.00","currency":"BRL"}.
func (m Money) MarshalJSON() ([]byte, error) {
	if m.currency == "" {
		return []byte("null"), nil
	}
	return []byte(`{"amount":"` + format(m.minor) + `","currency":"` + string(m.currency) + `"}`), nil
}

// UnmarshalJSON accepts the wire shape {"amount":"25.00","currency":"BRL"}.
// The amount goes through the same strict parser as the HTTP layer, so a
// float-shaped payload is rejected at the boundary.
func (m *Money) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*m = Money{}
		return nil
	}
	var raw struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	parsed, err := Parse(raw.Amount, Currency(raw.Currency))
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
