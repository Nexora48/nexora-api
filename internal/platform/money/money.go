// Package money keeps billing arithmetic in integer paise: no floats, one rounding rule (half up).
package money

import (
	"fmt"
	"strings"
)

// MulBP returns amount × basisPoints / 10000, rounded half up (150 bp = 1.5 %).
func MulBP(amount, bp int64) int64 {
	if amount <= 0 || bp <= 0 {
		return 0
	}
	return (amount*bp + 5000) / 10000
}

// Mul returns amount × qty (for a per-square-foot rate).
func Mul(rate int64, qty int) int64 { return rate * int64(qty) }

// Rupees formats paise as "₹1,23,456.50" (Indian digit grouping, paise shown only when non-zero).
func Rupees(paise int64) string {
	sign := ""
	if paise < 0 {
		sign, paise = "-", -paise
	}
	r, p := paise/100, paise%100
	s := fmt.Sprint(r)
	if len(s) > 3 {
		head, tail := s[:len(s)-3], s[len(s)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		s = strings.Join(parts, ",") + "," + tail
	}
	if p != 0 {
		return fmt.Sprintf("%s₹%s.%02d", sign, s, p)
	}
	return sign + "₹" + s
}

// Min returns the smaller of two amounts.
func Min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
