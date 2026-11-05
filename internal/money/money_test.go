package money

import "testing"

func TestFormatAndParseRoundTrip(t *testing.T) {
	cases := []struct {
		amount   int64
		currency string
		decimal  string
	}{
		{2500, "usd", "25.00"},
		{5, "usd", "0.05"},
		{0, "eur", "0.00"},
		{500, "jpy", "500"},
		{1234, "KWD", "1.234"},
	}
	for _, c := range cases {
		if got := Format(c.amount, c.currency); got != c.decimal {
			t.Errorf("Format(%d, %s) = %q, want %q", c.amount, c.currency, got, c.decimal)
		}
		if got, err := Parse(c.decimal, c.currency); err != nil || got != c.amount {
			t.Errorf("Parse(%q, %s) = %d, %v; want %d", c.decimal, c.currency, got, err, c.amount)
		}
	}
}

func TestParseAcceptsShortFractions(t *testing.T) {
	if got, err := Parse("25.5", "usd"); err != nil || got != 2550 {
		t.Errorf("Parse(25.5) = %d, %v; want 2550", got, err)
	}
	if got, err := Parse("25", "usd"); err != nil || got != 2500 {
		t.Errorf("Parse(25) = %d, %v; want 2500", got, err)
	}
}

func TestParseRejectsMalformedAmounts(t *testing.T) {
	for _, s := range []string{"", "-1.00", "+1.00", "1.001", "1.", ".50", "1e3", "1,00", "abc"} {
		if _, err := Parse(s, "usd"); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", s)
		}
	}
	if _, err := Parse("1.5", "jpy"); err == nil {
		t.Error("Parse(1.5, jpy) succeeded, want error")
	}
}
