package narrative

import "testing"

func TestPercentRoundsToTenthsHalfUp(t *testing.T) {
	cases := []struct {
		bp     int64
		plain  string
		signed string
	}{
		{0, "0%", "0%"},
		{4, "0%", "0%"},      // under a twentieth of a percent reads as zero, no sign
		{5, "0.1%", "+0.1%"}, // half a tenth rounds up
		{-5, minus + "0.1%", minus + "0.1%"},
		{910, "9.1%", "+9.1%"},
		{1_000, "10%", "+10%"}, // no trailing ".0"
		{-580, minus + "5.8%", minus + "5.8%"},
		{12_345, "123.5%", "+123.5%"},
		{-4, "0%", "0%"}, // a tiny decrease is not shown as negative zero
	}
	for _, c := range cases {
		if got := Percent(c.bp); got != c.plain {
			t.Errorf("Percent(%d) = %q, want %q", c.bp, got, c.plain)
		}
		if got := SignedPercent(c.bp); got != c.signed {
			t.Errorf("SignedPercent(%d) = %q, want %q", c.bp, got, c.signed)
		}
	}
}

func TestJoinListReadsAsASentence(t *testing.T) {
	cases := map[string][]string{
		"":                            nil,
		"forecast":                    {"forecast"},
		"forecast and coverage":       {"forecast", "coverage"},
		"cost, forecast and coverage": {"cost", "forecast", "coverage"},
	}
	for want, items := range cases {
		if got := joinList(items); got != want {
			t.Errorf("joinList(%v) = %q, want %q", items, got, want)
		}
	}
}
