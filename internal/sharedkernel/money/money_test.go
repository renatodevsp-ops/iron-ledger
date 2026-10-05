package money

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParse_accepted(t *testing.T) {
	tests := []struct {
		name   string
		amount string
		cur    Currency
		want   string
		minor  int64
	}{
		{"integer without fraction is normalised", "25", "BRL", "25.00", 2500},
		{"single fraction digit is padded", "25.5", "BRL", "25.50", 2550},
		{"two fraction digits are kept", "25.05", "BRL", "25.05", 2505},
		{"zero", "0", "BRL", "0.00", 0},
		{"explicit zero", "0.00", "BRL", "0.00", 0},
		{"negative", "-25.00", "BRL", "-25.00", -2500},
		{"negative fraction only", "-0.07", "BRL", "-0.07", -7},
		{"negative zero normalises to zero", "-0.00", "BRL", "0.00", 0},
		{"one cent", "0.01", "BRL", "0.01", 1},
		{"largest representable", "92233720368547758.07", "BRL", "92233720368547758.07", 9223372036854775807},
		{"smallest representable", "-92233720368547758.07", "BRL", "-92233720368547758.07", -9223372036854775807},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.amount, tt.cur)
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tt.amount, err)
			}
			if got.String() != tt.want {
				t.Errorf("String() = %q, want %q", got.String(), tt.want)
			}
			if got.AmountMinor() != tt.minor {
				t.Errorf("AmountMinor() = %d, want %d", got.AmountMinor(), tt.minor)
			}
			if got.Currency() != tt.cur {
				t.Errorf("Currency() = %q, want %q", got.Currency(), tt.cur)
			}
		})
	}
}

func TestParse_rejected(t *testing.T) {
	tests := []struct {
		name   string
		amount string
		cur    Currency
		target error
	}{
		{"empty", "", "BRL", ErrInvalidAmount},
		{"NaN", "NaN", "BRL", ErrInvalidAmount},
		{"Infinity", "Infinity", "BRL", ErrInvalidAmount},
		{"negative Infinity", "-Inf", "BRL", ErrInvalidAmount},
		{"scientific notation lower", "1e5", "BRL", ErrInvalidAmount},
		{"scientific notation upper", "1.5E+3", "BRL", ErrInvalidAmount},
		{"excess scale", "1.234", "BRL", ErrInvalidAmount},
		{"excess scale on an integer", "1.000", "BRL", ErrInvalidAmount},
		{"leading plus", "+1.00", "BRL", ErrInvalidAmount},
		{"bare sign", "-", "BRL", ErrInvalidAmount},
		{"bare decimal point", "1.", "BRL", ErrInvalidAmount},
		{"decimal point only", ".50", "BRL", ErrInvalidAmount},
		{"surrounding whitespace", " 1.00", "BRL", ErrInvalidAmount},
		{"trailing whitespace", "1.00 ", "BRL", ErrInvalidAmount},
		{"thousands separator", "1,000.00", "BRL", ErrInvalidAmount},
		{"currency symbol", "R$ 25.00", "BRL", ErrInvalidAmount},
		{"letters", "abc", "BRL", ErrInvalidAmount},
		{"double decimal point", "1.0.0", "BRL", ErrInvalidAmount},
		{"too many integer digits", "9223372036854775808.00", "BRL", ErrInvalidAmount},
		{"empty currency", "1.00", "", ErrInvalidCurrency},
		{"lowercase currency", "1.00", "brl", ErrInvalidCurrency},
		{"unknown currency", "1.00", "XYZ", ErrInvalidCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.amount, tt.cur)
			if err == nil {
				t.Fatalf("Parse(%q, %q) = %v, want error", tt.amount, tt.cur, got)
			}
			if !errors.Is(err, tt.target) {
				t.Errorf("Parse(%q, %q) error = %v, want %v", tt.amount, tt.cur, err, tt.target)
			}
		})
	}
}

func TestParse_isNotSilentlyRounded(t *testing.T) {
	// An input the platform cannot represent exactly must be refused, never
	// rounded into a different monetary value.
	for _, amount := range []string{"0.005", "25.999", "1e-2", "1.2.3"} {
		if _, err := Parse(amount, "BRL"); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalidAmount", amount, err)
		}
	}
}

func TestZero(t *testing.T) {
	z := Zero("BRL")
	if !z.IsZero() {
		t.Errorf("Zero(BRL).IsZero() = false")
	}
	if z.String() != "0.00" {
		t.Errorf("Zero(BRL).String() = %q, want 0.00", z.String())
	}
	if !z.Valid() {
		t.Errorf("Zero(BRL).Valid() = false, want true")
	}
	var uninitialised Money
	if uninitialised.Valid() {
		t.Errorf("zero Money.Valid() = true, want false")
	}
	if _, err := uninitialised.Add(Zero("BRL")); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("uninitialised Money.Add error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestAdd(t *testing.T) {
	tests := []struct {
		name  string
		a, b  string
		want  string
		isErr error
	}{
		{name: "same currency", a: "10.00", b: "15.00", want: "25.00"},
		{name: "carries are exact, not rounded", a: "0.01", b: "0.02", want: "0.03"},
		{name: "crossing zero", a: "-5.00", b: "5.00", want: "0.00"},
		{name: "different currencies", a: "10.00", b: "15.00", isErr: ErrCurrencyMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := MustParse(tt.a, "BRL")
			b := MustParse(tt.b, "BRL")
			if tt.isErr == ErrCurrencyMismatch {
				b = MustParse(tt.b, "USD")
			}
			got, err := a.Add(b)
			if tt.isErr != nil {
				if !errors.Is(err, tt.isErr) {
					t.Fatalf("Add error = %v, want %v", err, tt.isErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Add unexpected error: %v", err)
			}
			if got.String() != tt.want {
				t.Errorf("Add = %q, want %q", got.String(), tt.want)
			}
		})
	}
}

func TestAdd_overflow(t *testing.T) {
	max := FromMinor(9223372036854775807, "BRL")
	if _, err := max.Add(FromMinor(1, "BRL")); !errors.Is(err, ErrOverflow) {
		t.Errorf("max.Add(1) error = %v, want ErrOverflow", err)
	}
	min := FromMinor(-9223372036854775807-1, "BRL")
	if _, err := min.Add(FromMinor(-1, "BRL")); !errors.Is(err, ErrOverflow) {
		t.Errorf("min.Add(-1) error = %v, want ErrOverflow", err)
	}
}

func TestSub(t *testing.T) {
	a := MustParse("100.00", "BRL")
	b := MustParse("80.00", "BRL")
	got, err := a.Sub(b)
	if err != nil {
		t.Fatalf("Sub unexpected error: %v", err)
	}
	if got.String() != "20.00" {
		t.Errorf("Sub = %q, want 20.00", got.String())
	}
	if _, err := a.Sub(MustParse("1.00", "USD")); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub across currencies error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := FromMinor(9223372036854775807, "BRL").Sub(FromMinor(-1, "BRL")); !errors.Is(err, ErrOverflow) {
		t.Errorf("max.Sub(-1) error = %v, want ErrOverflow", err)
	}
}

func TestNeg(t *testing.T) {
	got, err := MustParse("25.00", "BRL").Neg()
	if err != nil {
		t.Fatalf("Neg unexpected error: %v", err)
	}
	if got.String() != "-25.00" {
		t.Errorf("Neg = %q, want -25.00", got.String())
	}
	if _, err := FromMinor(-9223372036854775807-1, "BRL").Neg(); !errors.Is(err, ErrOverflow) {
		t.Errorf("Neg of min int64 error = %v, want ErrOverflow", err)
	}
}

func TestCmpAndEqual(t *testing.T) {
	a := MustParse("25.00", "BRL")
	b := MustParse("25.01", "BRL")
	usd := MustParse("25.00", "USD")

	if got, err := a.Cmp(b); err != nil || got != -1 {
		t.Errorf("a.Cmp(b) = %d, %v; want -1, nil", got, err)
	}
	if got, err := b.Cmp(a); err != nil || got != 1 {
		t.Errorf("b.Cmp(a) = %d, %v; want 1, nil", got, err)
	}
	if got, err := a.Cmp(a); err != nil || got != 0 {
		t.Errorf("a.Cmp(a) = %d, %v; want 0, nil", got, err)
	}
	if _, err := a.Cmp(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Cmp across currencies error = %v, want ErrCurrencyMismatch", err)
	}
	if a.Equal(usd) {
		t.Error("a.Equal(usd) = true, want false: same number, different currency")
	}
	if !a.Equal(MustParse("25.00", "BRL")) {
		t.Error("a.Equal(25.00 BRL) = false, want true")
	}
}

func TestSigns(t *testing.T) {
	if !MustParse("0.01", "BRL").IsPositive() {
		t.Error("0.01 should be positive")
	}
	if MustParse("0.01", "BRL").IsZero() {
		t.Error("0.01 should not be zero")
	}
	if !MustParse("-0.01", "BRL").IsNegative() {
		t.Error("-0.01 should be negative")
	}
	if !MustParse("0.00", "BRL").IsZero() {
		t.Error("0.00 should be zero")
	}
}

func TestAbs(t *testing.T) {
	if got := MustParse("-3.50", "BRL").Abs(); got.String() != "3.50" {
		t.Errorf("Abs = %q, want 3.50", got.String())
	}
}

func TestJSON_roundTrip(t *testing.T) {
	type payload struct {
		Money Money `json:"money"`
	}
	original := payload{Money: MustParse("-1234.56", "BRL")}

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `{"money":{"amount":"-1234.56","currency":"BRL"}}`; string(raw) != want {
		t.Errorf("Marshal = %s, want %s", raw, want)
	}

	var decoded payload
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !decoded.Money.Equal(original.Money) {
		t.Errorf("round trip = %v, want %v", decoded.Money, original.Money)
	}
}

func TestJSON_rejectsFloatShapedPayloads(t *testing.T) {
	for _, raw := range []string{
		`{"money":{"amount":25.00,"currency":"BRL"}}`,
		`{"money":{"amount":"1e3","currency":"BRL"}}`,
		`{"money":{"amount":"1.234","currency":"BRL"}}`,
		`{"money":{"amount":"","currency":"BRL"}}`,
		`{"money":{"amount":"1.00","currency":"XXX"}}`,
		`{"money":{"amount":"1.00"}}`,
	} {
		var decoded struct {
			Money Money `json:"money"`
		}
		if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
			t.Errorf("Unmarshal(%s) succeeded, want error", raw)
		}
	}
}

func TestMustParse_panicsOnInvalidInput(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustParse did not panic")
		}
	}()
	MustParse("1.234", "BRL")
}
