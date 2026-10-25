package money

import "testing"

func TestMulBPRoundsHalfUp(t *testing.T) {
	for _, c := range []struct{ amount, bp, want int64 }{
		{100000, 150, 1500}, // 1.5 % of ₹1000
		{333, 150, 5},       // 4.995 paise -> 5
		{100, 50, 1},        // 0.5 paise -> 1 (half up)
		{99, 50, 0},         // 0.495 -> 0
		{0, 150, 0},
		{100000, 0, 0},
		{-5, 100, 0},
	} {
		if got := MulBP(c.amount, c.bp); got != c.want {
			t.Errorf("MulBP(%d,%d)=%d want %d", c.amount, c.bp, got, c.want)
		}
	}
}

func TestRupeesUsesIndianGrouping(t *testing.T) {
	for p, want := range map[int64]string{
		0: "₹0", 100: "₹1", 99: "₹0.99", 420000: "₹4,200", 12345650: "₹1,23,456.50", 100000000: "₹10,00,000", -150: "-₹1.50", 99999: "₹999.99",
	} {
		if got := Rupees(p); got != want {
			t.Errorf("Rupees(%d)=%q want %q", p, got, want)
		}
	}
}

func TestMulAndMin(t *testing.T) {
	if Mul(250, 1200) != 300000 || Min(3, 5) != 3 || Min(5, 3) != 3 {
		t.Fatal("arithmetic")
	}
}
