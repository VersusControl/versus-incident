// Package narrative turns computed FinOps findings into sentences.
//
// It owns every user-facing word of the FinOps page, the report, the copied
// summary and the alert text, in two depths: Summary (plain words, no provider
// jargon) and Details (technical labels and identifiers). It never computes a
// number: callers pass already computed amounts in integer micro-units, and
// the templates only format and arrange them. A clause that has no data behind
// it is dropped rather than guessed.
//
// The package is a leaf: it imports only the standard library so the FinOps
// core, the HTTP layer and the agent tools can all share it.
package narrative

import (
	"strconv"
	"strings"
	"time"
)

// Depth selects the wording: Summary for managers, Details for engineers.
// The depth changes labels and identifiers, never numbers.
type Depth string

const (
	Summary Depth = "summary"
	Details Depth = "details"
)

// ParseDepth maps a query value to a Depth. Anything but "details" is Summary.
func ParseDepth(s string) Depth {
	if strings.EqualFold(strings.TrimSpace(s), string(Details)) {
		return Details
	}
	return Summary
}

const micro = 1_000_000

// minus is the typographic minus sign used for negative amounts.
const minus = "−"

// ShowsPositive reports that an amount prints as more than zero: anything
// under half a cent prints as "$0" and so cannot back "went up $X".
func ShowsPositive(micros int64) bool { return micros >= 5_000 }

// Money formats an amount in micro-units. Amounts of 100 or more, and whole
// amounts, are shown without cents ("$43,520", "$50"); smaller amounts keep
// two decimals ("$0.20"). Rounding is half away from zero and happens here
// only, for display.
func Money(micros int64, currency string) string {
	return moneyString(micros, currency, false)
}

// SignedMoney is Money with an explicit sign ("+$3,640", "−$2,310").
// Zero has no sign.
func SignedMoney(micros int64, currency string) string {
	return moneyString(micros, currency, true)
}

func moneyString(micros int64, currency string, signed bool) string {
	neg := micros < 0
	abs := uint64(micros)
	if neg {
		abs = uint64(-(micros + 1)) + 1
	}
	cents := (abs + 5_000) / 10_000
	var body string
	if abs >= 100*micro || cents%100 == 0 {
		dollars := (abs + micro/2) / micro
		body = groupThousands(strconv.FormatUint(dollars, 10))
	} else {
		body = groupThousands(strconv.FormatUint(cents/100, 10)) + "." + pad2(cents%100)
	}
	sym, suffix := currencySymbol(currency)
	out := sym + body + suffix
	if body == "0" || body == "0.00" {
		return out
	}
	switch {
	case neg:
		return minus + out
	case signed:
		return "+" + out
	}
	return out
}

func currencySymbol(code string) (prefix, suffix string) {
	switch strings.ToUpper(code) {
	case "", "USD":
		return "$", ""
	case "EUR":
		return "€", ""
	case "GBP":
		return "£", ""
	}
	return "", " " + strings.ToUpper(code)
}

func pad2(n uint64) string {
	if n < 10 {
		return "0" + strconv.FormatUint(n, 10)
	}
	return strconv.FormatUint(n, 10)
}

func groupThousands(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	head := len(digits) % 3
	if head > 0 {
		b.WriteString(digits[:head])
	}
	for i := head; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// Percent formats basis points (1/100 of a percent) with one decimal:
// 910 → "9.1%". Negative values use the minus sign.
func Percent(bp int64) string {
	return percentString(bp, false)
}

// SignedPercent is Percent with an explicit sign ("+9.1%").
func SignedPercent(bp int64) string {
	return percentString(bp, true)
}

func percentString(bp int64, signed bool) string {
	neg := bp < 0
	if neg {
		bp = -bp
	}
	tenths := (bp + 5) / 10
	body := strconv.FormatInt(tenths/10, 10)
	if tenths%10 != 0 {
		body += "." + strconv.FormatInt(tenths%10, 10)
	}
	body += "%"
	if tenths == 0 {
		return body
	}
	if neg {
		return minus + body
	}
	if signed {
		return "+" + body
	}
	return body
}

// ShortDate formats a day as "Oct 9".
func ShortDate(t time.Time) string {
	return t.Format("Jan 2")
}

// MonthName formats a month as "October".
func MonthName(m time.Month) string {
	return m.String()
}

// Direction words for the change card.
const (
	Up   = "up"
	Down = "down"
	Same = "same"
)

// SameBandBP is the band (±2%) inside which a change reads "About the same".
const SameBandBP = 200

// DirectionOf classifies a change given in basis points of the base total.
func DirectionOf(changeBP int64) string {
	switch {
	case changeBP > SameBandBP:
		return Up
	case changeBP < -SameBandBP:
		return Down
	}
	return Same
}

// ChangeWord is the card word for a direction.
func ChangeWord(direction string) string {
	switch direction {
	case Up:
		return "Up"
	case Down:
		return "Down"
	}
	return "About the same"
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	// Keep acronyms ("NAT", "EBS") as written.
	if len(r) > 1 && strings.ToUpper(string(r[:2])) == string(r[:2]) {
		return s
	}
	r[0] = []rune(strings.ToLower(string(r[0])))[0]
	return string(r)
}

// joinList joins items as "a", "a and b", "a, b and c".
func joinList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
