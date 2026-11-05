// Package money converts between minor units and the decimal strings some
// provider APIs use.
package money

import (
	"fmt"
	"strconv"
	"strings"
)

// exponents lists ISO 4217 currencies whose minor unit is not 1/100.
var exponents = map[string]int{
	"bif": 0, "clp": 0, "djf": 0, "gnf": 0, "jpy": 0, "kmf": 0, "krw": 0, "mga": 0,
	"pyg": 0, "rwf": 0, "ugx": 0, "vnd": 0, "vuv": 0, "xaf": 0, "xof": 0, "xpf": 0,
	"bhd": 3, "jod": 3, "kwd": 3, "omr": 3, "tnd": 3,
}

// Exponent returns the number of decimal places in currency's minor unit.
func Exponent(currency string) int {
	if e, ok := exponents[strings.ToLower(currency)]; ok {
		return e
	}
	return 2
}

// Format renders a non-negative minor-unit amount as a decimal string:
// Format(2500, "usd") is "25.00" and Format(500, "jpy") is "500".
func Format(amount int64, currency string) string {
	exp := Exponent(currency)
	digits := strconv.FormatInt(amount, 10)
	if exp == 0 {
		return digits
	}
	if pad := exp + 1 - len(digits); pad > 0 {
		digits = strings.Repeat("0", pad) + digits
	}
	return digits[:len(digits)-exp] + "." + digits[len(digits)-exp:]
}

// Parse converts a decimal string to minor units. It rejects signs, exponents
// and more decimal places than the currency has.
func Parse(s, currency string) (int64, error) {
	exp := Exponent(currency)
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" || !allDigits(whole) || !allDigits(frac) || len(frac) > exp || (hasFrac && frac == "") {
		return 0, fmt.Errorf("invalid %s amount %q", strings.ToUpper(currency), s)
	}
	n, err := strconv.ParseInt(whole+frac+strings.Repeat("0", exp-len(frac)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s amount %q: %w", strings.ToUpper(currency), s, err)
	}
	return n, nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
