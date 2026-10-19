package value

import "strings"

// validDecimal reports whether s is a plain decimal number: an optional sign,
// then at least one digit, with at most one decimal point. No exponent.
func validDecimal(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '+' || s[0] == '-' {
		i++
	}
	digits, dot := 0, false
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case isDigit(c):
			digits++
		case c == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return digits > 0
}

// decimalDigits returns the powers of ten of the most and the least
// significant non-zero digits of a valid decimal. For "120.05" it returns
// 2 and -2; for "0.0012", -3 and -4; for "1200", 3 and 2. zero is true when
// every digit is zero.
func decimalDigits(s string) (msd, lsd int, zero bool) {
	s = strings.TrimLeft(s, "+-")
	whole, frac, _ := strings.Cut(s, ".")
	whole = strings.TrimLeft(whole, "0")
	frac = strings.TrimRight(frac, "0")
	if whole == "" && frac == "" {
		return 0, 0, true
	}
	if whole != "" {
		msd = len(whole) - 1
	} else {
		msd = -(len(frac) - len(strings.TrimLeft(frac, "0")) + 1)
	}
	if frac != "" {
		lsd = -len(frac)
	} else {
		lsd = len(whole) - len(strings.TrimRight(whole, "0"))
	}
	return msd, lsd, false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
