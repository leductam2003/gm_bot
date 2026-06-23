package api

import "testing"

func TestParseUnits(t *testing.T) {
	cases := []struct {
		in       string
		decimals int
		want     string // expected base-units; "" means an error is expected
	}{
		{"0.05", 18, "50000000000000000"},
		{"1", 18, "1000000000000000000"},
		{"0.000000000000000001", 18, "1"}, // 1 wei
		{".5", 18, "500000000000000000"},
		{"1.", 18, "1000000000000000000"},
		{"100", 0, "100"},
		{"1.0", 0, "1"},          // trailing-zero fraction is fine for decimals=0
		{"1.234567", 6, "1234567"},
		// rejected — these previously produced silent wrong amounts or are malformed
		{"+5", 18, ""},
		{" +5 ", 18, ""},
		{"0.+5", 18, ""},
		{"1e2", 18, ""},
		{"1.5e2", 18, ""},
		{"-1", 18, ""},
		{"abc", 18, ""},
		{"1.2.3", 18, ""},
		{"0", 18, ""},          // must be > 0
		{"0.0", 18, ""},        // must be > 0
		{"1.5", 0, ""},         // would silently truncate to 1
		{"1.2345678", 6, ""},   // 7 fractional digits for a 6-decimal token
	}
	for _, c := range cases {
		got, err := parseUnits(c.in, c.decimals)
		if c.want == "" {
			if err == nil {
				t.Errorf("parseUnits(%q,%d) = %s, want error", c.in, c.decimals, got.String())
			}
			continue
		}
		if err != nil {
			t.Errorf("parseUnits(%q,%d) unexpected error: %v", c.in, c.decimals, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("parseUnits(%q,%d) = %s, want %s", c.in, c.decimals, got.String(), c.want)
		}
	}
}
