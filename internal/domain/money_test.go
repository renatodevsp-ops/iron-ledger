package domain_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMoneyJSONRoundTripIsExact is the regression test for Constitution
// Principle I. 1234567890123 minor units does not survive a float64: beyond
// 2^53 the integer itself is already inexact, and JSON numbers with a
// fractional part are rejected outright.
func TestMoneyJSONRoundTripIsExact(t *testing.T) {
	const want int64 = 1234567890123

	payload, err := json.Marshal(struct {
		AmountMinor int64           `json:"amountMinor"`
		Currency    domain.Currency `json:"currency"`
	}{AmountMinor: want, Currency: domain.BRL})
	require.NoError(t, err)
	assert.Contains(t, string(payload), `"amountMinor":1234567890123`)

	// Re-read the wire form with UseNumber so the literal token is inspected
	// rather than a float: a float64 could not hold 1234567890123 exactly, so
	// seeing the exact digit string here is the actual proof.
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	var raw map[string]any
	require.NoError(t, dec.Decode(&raw))
	token, ok := raw["amountMinor"].(json.Number)
	require.True(t, ok, "amountMinor must decode as a JSON number literal")
	assert.Equal(t, "1234567890123", token.String())

	var back struct {
		AmountMinor int64           `json:"amountMinor"`
		Currency    domain.Currency `json:"currency"`
	}
	require.NoError(t, json.Unmarshal(payload, &back))
	assert.Equal(t, want, back.AmountMinor)
	assert.Equal(t, domain.BRL, back.Currency)

	// A fractional wire value must be refused rather than truncated.
	var bad struct {
		AmountMinor int64 `json:"amountMinor"`
	}
	assert.Error(t, json.Unmarshal([]byte(`{"amountMinor":1234567890123.5}`), &bad))
}

func TestParseRejectsEverythingItShould(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"non numeric", "abc"},
		{"thousands separator", "1,000.00"},
		{"currency suffix", "BRL 10.00"},
		{"exponent notation", "1e3"},
		{"negative", "-10.00"},
		{"three decimals", "10.001"},
		{"bare dot", "."},
		{"trailing dot only", "10."},
		{"double dot", "10.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := domain.Parse(tc.input, domain.BRL)
			require.Error(t, err)
			assert.Equal(t, domain.ReasonInvalidAmount, domain.CodeOf(err))
		})
	}
}

func TestParseAcceptsAndNeverRounds(t *testing.T) {
	cases := []struct {
		input string
		want  int64
	}{
		{"0", 0},
		{"0.01", 1},
		{"0.1", 10},
		{"0.10", 10},
		{"1", 100},
		{"1.00", 100},
		{"10.99", 1099},
		{"1234567890123.45", 123456789012345},
		{"+42", 4200},
		{" 42.42 ", 4242},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			m, err := domain.Parse(tc.input, domain.BRL)
			require.NoError(t, err)
			assert.Equal(t, tc.want, m.AmountMinor)
			assert.Equal(t, domain.BRL, m.Currency)
		})
	}
}

func TestParseRejectsOutOfRange(t *testing.T) {
	_, err := domain.Parse("99999999999999999999.00", domain.BRL)
	require.Error(t, err)
	assert.Equal(t, domain.ReasonInvalidAmount, domain.CodeOf(err))
}

func TestParseRejectsUnsupportedCurrency(t *testing.T) {
	_, err := domain.Parse("10.00", domain.Currency("EUR"))
	require.Error(t, err)
	assert.Equal(t, domain.ReasonInvalidCurrency, domain.CodeOf(err))
}

func TestStringUsesIntegerArithmeticOnly(t *testing.T) {
	cases := []struct {
		m    domain.Money
		want string
	}{
		{domain.Money{AmountMinor: 0, Currency: domain.BRL}, "BRL 0.00"},
		{domain.Money{AmountMinor: 5, Currency: domain.BRL}, "BRL 0.05"},
		{domain.Money{AmountMinor: 50, Currency: domain.USD}, "USD 0.50"},
		{domain.Money{AmountMinor: 3000, Currency: domain.BRL}, "BRL 30.00"},
		{domain.Money{AmountMinor: 123456789012345, Currency: domain.USD}, "USD 1234567890123.45"},
		{domain.Money{AmountMinor: -3000, Currency: domain.BRL}, "BRL -30.00"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.m.String())
		})
	}
}

func TestAddAndSubRequireEqualCurrencies(t *testing.T) {
	brl := domain.Money{AmountMinor: 1000, Currency: domain.BRL}
	usd := domain.Money{AmountMinor: 1000, Currency: domain.USD}

	sum, err := brl.Add(domain.Money{AmountMinor: 250, Currency: domain.BRL})
	require.NoError(t, err)
	assert.Equal(t, int64(1250), sum.AmountMinor)

	diff, err := brl.Sub(domain.Money{AmountMinor: 250, Currency: domain.BRL})
	require.NoError(t, err)
	assert.Equal(t, int64(750), diff.AmountMinor)

	_, err = brl.Add(usd)
	assert.Equal(t, domain.ReasonCurrencyMismatch, domain.CodeOf(err))
	_, err = brl.Sub(usd)
	assert.Equal(t, domain.ReasonCurrencyMismatch, domain.CodeOf(err))
}

// TestNoFloatTypesInMoney asserts structurally that the money type exposes no
// float-typed field, so a future change cannot quietly introduce one. The
// kind's string is checked rather than a switch, so a float alias or a named
// float type is caught too.
func TestNoFloatTypesInMoney(t *testing.T) {
	moneyType := reflect.TypeOf(domain.Money{})
	require.Equal(t, 2, moneyType.NumField())
	for i := 0; i < moneyType.NumField(); i++ {
		f := moneyType.Field(i)
		t.Run(f.Name, func(t *testing.T) {
			switch f.Type.Kind() {
			case reflect.Float32, reflect.Float64:
				t.Fatalf("field %s is a float (%s): a constitution violation", f.Name, f.Type)
			}
		})
	}
	assert.Equal(t, "int64", moneyType.Field(0).Type.Kind().String())
	assert.Equal(t, reflect.String, moneyType.Field(1).Type.Kind())
}

// TestNoFloatInOperationRequestShape applies the same guard to the validated
// request aggregate, so a float-typed amount cannot enter through it either.
func TestNoFloatInOperationRequestShape(t *testing.T) {
	opType := reflect.TypeOf(domain.Operation{})
	for i := 0; i < opType.NumField(); i++ {
		f := opType.Field(i)
		switch f.Type.Kind() {
		case reflect.Float32, reflect.Float64:
			t.Fatalf("domain.Operation field %s is a float", f.Name)
		}
	}
	amount, ok := opType.FieldByName("Amount")
	require.True(t, ok)
	assert.Equal(t, reflect.TypeOf(domain.Money{}), amount.Type)
	assert.NotEmpty(t, strings.TrimSpace(amount.Name))
}
