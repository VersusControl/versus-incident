package finops

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Micros is an amount in millionths of the currency unit. Money is never a
// float: provider strings are parsed into Micros, sums are exact integer
// sums, and rounding happens only for display (see the narrative package).
type Micros = int64

// MicrosPerUnit is one currency unit (one dollar) in Micros.
const MicrosPerUnit Micros = 1_000_000

// USD converts whole currency units to Micros; for config and fixtures.
func USD(units int64) Micros { return units * MicrosPerUnit }

// ErrMoneyOverflow means an amount does not fit int64 micro-units.
var ErrMoneyOverflow = errors.New("finops: amount out of range")

// ErrAmountOverflow reports a sum of amounts that does not fit Micros: the
// service's closed class for an amount it cannot represent. A view returns it
// instead of a wrapped total.
var ErrAmountOverflow = fmt.Errorf("%w: a sum over these amounts does not fit the readable range", ErrMoneyOverflow)

// addMagnitudes adds one amount to the running positive and negative
// magnitudes. Any subset sum of the amounts lies between the negative
// magnitude (negated) and the positive one, so when both fit, no sum over
// these lines can wrap. It reports false when one of them cannot fit.
// Magnitudes are unsigned because the smallest amount is one past the
// largest int64.
func addMagnitudes(positive, negative uint64, amount Micros) (uint64, uint64, bool) {
	if amount >= 0 {
		if uint64(amount) > uint64(math.MaxInt64)-positive {
			return 0, 0, false
		}
		return positive + uint64(amount), negative, true
	}
	magnitude := uint64(-(amount + 1)) + 1
	if magnitude > uint64(math.MaxInt64)+1-negative {
		return 0, 0, false
	}
	return positive, negative + magnitude, true
}

// ParseMicros parses a decimal string as reported by billing APIs
// ("123.4567891", "-0.01", "1.2E-7", "  42 ") into micro-units. Digits past
// the sixth decimal are rounded half away from zero. It never goes through a
// float.
func ParseMicros(s string) (Micros, error) {
	return parseScaled(s, 6)
}

// ParseQuantityMicros parses a usage quantity the same way (millionths of
// a unit).
func ParseQuantityMicros(s string) (int64, error) { return parseScaled(s, 6) }

func parseScaled(raw string, scale int) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("finops: empty amount")
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	exp := 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e > 400 || e < -400 {
			return 0, fmt.Errorf("finops: invalid amount %q", raw)
		}
		exp, s = e, s[:i]
	}
	intPart, frac, _ := strings.Cut(s, ".")
	digits := intPart + frac
	if digits == "" || strings.ContainsFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, fmt.Errorf("finops: invalid amount %q", raw)
	}
	// value = digits × 10^(exp − len(frac)); we want value × 10^scale.
	shift := exp - len(frac) + scale
	n := new(big.Int)
	n.SetString(digits, 10)
	ten := big.NewInt(10)
	if shift >= 0 {
		n.Mul(n, new(big.Int).Exp(ten, big.NewInt(int64(shift)), nil))
	} else {
		div := new(big.Int).Exp(ten, big.NewInt(int64(-shift)), nil)
		q, r := new(big.Int).QuoRem(n, div, new(big.Int))
		if r.Lsh(r, 1).Cmp(div) >= 0 {
			q.Add(q, big.NewInt(1))
		}
		n = q
	}
	if neg {
		n.Neg(n)
	}
	if !n.IsInt64() {
		return 0, ErrMoneyOverflow
	}
	return n.Int64(), nil
}

// FormatMicros renders an exact decimal string ("12.345678"), for CSV and
// debugging. Display rounding belongs to the narrative package.
func FormatMicros(m Micros) string {
	neg := m < 0
	u := uint64(m)
	if neg {
		u = uint64(-(m + 1)) + 1
	}
	s := strconv.FormatUint(u/1_000_000, 10) + "." + fmt.Sprintf("%06d", u%1_000_000)
	if neg {
		return "-" + s
	}
	return s
}

// mulDiv returns round(a×b/c) half away from zero without overflow; c ≠ 0.
// The result saturates at the int64 range.
func mulDiv(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	d := big.NewInt(c)
	neg := (n.Sign() < 0) != (d.Sign() < 0)
	n.Abs(n)
	d.Abs(d)
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	if r.Lsh(r, 1).Cmp(d) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	if !q.IsInt64() {
		if q.Sign() < 0 {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return q.Int64()
}

// basisPoints returns part/whole in basis points (1/100 of a percent),
// rounded; 0 when whole is 0.
func basisPoints(part, whole int64) int64 {
	if whole == 0 {
		return 0
	}
	if whole < 0 {
		whole = -whole
	}
	return mulDiv(part, 10_000, whole)
}
