package eval

import "testing"

func TestFormatUSD(t *testing.T) {
	cases := []struct {
		micro int64
		want  string
	}{
		{0, "$0.00"},
		{7, "$0.000007"},       // a tiny LLM call must not round away to $0.000
		{2000, "$0.002"},       // judge spend
		{31000, "$0.031"},      // typical per-case cost
		{500000, "$0.50"},      // half a dollar keeps two decimals
		{5000000, "$5.00"},     // a whole-dollar budget stays short
		{1234567, "$1.234567"}, // full micro precision when it is there
		{10500000, "$10.50"},   // larger amount, trimmed
	}
	for _, tc := range cases {
		if got := FormatUSD(tc.micro); got != tc.want {
			t.Errorf("FormatUSD(%d) = %q, want %q", tc.micro, got, tc.want)
		}
	}
}
