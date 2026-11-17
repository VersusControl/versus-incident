package finops

import "testing"

func TestParseQuantityMicros(t *testing.T) {
	cases := map[string]int64{
		"4250":       4_250_000_000,
		"0.0000015":  2,
		"0.0000014":  1,
		"1.5E2":      150_000_000,
		"-3":         -3_000_000,
		"  0.125  ":  125_000,
		"0.00000000": 0,
	}
	for in, want := range cases {
		got, err := ParseQuantityMicros(in)
		if err != nil || got != want {
			t.Errorf("ParseQuantityMicros(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "N/A", "1,000", "12 GB"} {
		if _, err := ParseQuantityMicros(bad); err == nil {
			t.Errorf("ParseQuantityMicros(%q) should fail", bad)
		}
	}
}

// Billing strings that are not plain decimals are refused, never read as
// zero or as a partial number.
func TestParseMicrosRefusesNonDecimals(t *testing.T) {
	for _, bad := range []string{"-", "+", ".", "-.", "+-1", "NaN", "Inf", "-Infinity", "0x10", "1_000", "1,000.00",
		"1 000", "e5", "1e", "1e+", "1e3.5", "$12", "12USD", "\t"} {
		if v, err := ParseMicros(bad); err == nil {
			t.Errorf("ParseMicros(%q) = %d, want an error", bad, v)
		}
	}
	for in, want := range map[string]Micros{"-.5": -500_000, "1e+3": 1_000_000_000, "-0.0000004": 0, "1E-400": 0, "00012.5": 12_500_000} {
		if got, err := ParseMicros(in); err != nil || got != want {
			t.Errorf("ParseMicros(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseMicros("1e400"); err == nil {
		t.Error("an amount past int64 must fail")
	}
}

func TestUSDIsWholeUnits(t *testing.T) {
	if USD(0) != 0 || USD(3) != 3_000_000 || USD(-2) != -2_000_000 {
		t.Fatal("USD converts whole units to micro-units")
	}
}
