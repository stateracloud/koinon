// SPDX-License-Identifier: Apache-2.0

package scrub

import "strings"

// findPersonal appends the spans of the rules that apply at LevelRedacted only.
func findPersonal(in string, spans []span) []span {
	spans = findEmails(in, spans)
	spans = findIBANs(in, spans)
	spans = findNumbers(in, spans)
	spans = findTokens(in, spans)
	return spans
}

func isDigit(b byte) bool { return '0' <= b && b <= '9' }
func isAlnum(b byte) bool {
	return isDigit(b) || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// findEmails finds addr-spec shapes around each '@': a local part of [A-Za-z0-9._%+-] and a
// domain of dot-separated labels ending in a letters-only label of two or more characters.
// "pkg@1.2.3" (no letters-only last label) and "@scope/pkg" (no local part) are not emails.
func findEmails(in string, spans []span) []span {
	for from := 0; ; {
		at := strings.IndexByte(in[from:], '@')
		if at < 0 {
			return spans
		}
		at += from
		from = at + 1
		ls := at
		for ls > 0 && isLocal(in[ls-1]) {
			ls--
		}
		for ls < at && (in[ls] == '.' || in[ls] == '-') { // a local part starts with an alphanumeric
			ls++
		}
		if ls == at {
			continue
		}
		de := at + 1
		for de < len(in) && (isAlnum(in[de]) || in[de] == '.' || in[de] == '-') {
			de++
		}
		for de > at+1 && (in[de-1] == '.' || in[de-1] == '-') { // trailing sentence punctuation
			de--
		}
		domain := in[at+1 : de]
		dot := strings.LastIndexByte(domain, '.')
		if dot <= 0 || !isTLD(domain[dot+1:]) || strings.Contains(domain, "..") {
			continue
		}
		spans = append(spans, span{ls, de, KindEmail})
		from = de
	}
}

func isLocal(b byte) bool {
	return isAlnum(b) || b == '.' || b == '_' || b == '%' || b == '+' || b == '-'
}

func isTLD(s string) bool {
	if len(s) < 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isAlpha(s[i]) {
			return false
		}
	}
	return true
}

// ibanLen is the length of an IBAN by country (SWIFT IBAN registry).
var ibanLen = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16, "BG": 22, "BH": 22, "BR": 29,
	"BY": 28, "CH": 21, "CR": 22, "CY": 28, "CZ": 24, "DE": 22, "DK": 18, "DO": 28, "EE": 20, "EG": 29,
	"ES": 24, "FI": 18, "FO": 18, "FR": 27, "GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27, "GT": 28,
	"HR": 21, "HU": 28, "IE": 22, "IL": 23, "IQ": 23, "IS": 26, "IT": 27, "JO": 30, "KW": 30, "KZ": 20,
	"LB": 28, "LC": 32, "LI": 21, "LT": 20, "LU": 20, "LV": 21, "LY": 25, "MC": 27, "MD": 24, "ME": 22,
	"MK": 19, "MR": 27, "MT": 31, "MU": 30, "NL": 18, "NO": 15, "PK": 24, "PL": 28, "PS": 29, "PT": 25,
	"QA": 29, "RO": 24, "RS": 22, "SA": 24, "SC": 31, "SE": 24, "SI": 19, "SK": 24, "SM": 27, "ST": 25,
	"SV": 28, "TL": 23, "TN": 24, "TR": 26, "UA": 29, "VA": 22, "VG": 24, "XK": 20,
}

// findIBANs finds an upper-case IBAN, written compactly or in groups separated by single spaces,
// and keeps it only when its length matches its country and ISO 7064 mod 97-10 holds.
func findIBANs(in string, spans []span) []span {
	for i := 0; i+4 <= len(in); i++ {
		if !isUpper(in[i]) || !isUpper(in[i+1]) || !isDigit(in[i+2]) || !isDigit(in[i+3]) {
			continue
		}
		if i > 0 && isAlnum(in[i-1]) {
			continue
		}
		want, ok := ibanLen[in[i:i+2]]
		if !ok {
			continue
		}
		var buf [34]byte
		n, j := 0, i
		for j < len(in) && n < want {
			c := in[j]
			if c == ' ' && n > 0 && j+1 < len(in) && isAlnum(in[j+1]) {
				j++
				continue
			}
			if !isDigit(c) && !isUpper(c) {
				break
			}
			buf[n] = c
			n++
			j++
		}
		if n != want || (j < len(in) && isAlnum(in[j])) || !mod97(buf[:n]) {
			continue
		}
		spans = append(spans, span{i, j, KindIBAN})
		i = j - 1
	}
	return spans
}

func isAlpha(b byte) bool { return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') }

func isUpper(b byte) bool { return 'A' <= b && b <= 'Z' }

// mod97 checks an IBAN: move the first four characters to the end, map letters to 10..35 and the
// number must leave remainder 1 modulo 97.
func mod97(iban []byte) bool {
	rem := 0
	step := func(c byte) {
		if isDigit(c) {
			rem = (rem*10 + int(c-'0')) % 97
			return
		}
		v := int(c-'A') + 10
		rem = (rem*100 + v) % 97
	}
	for _, c := range iban[4:] {
		step(c)
	}
	for _, c := range iban[:4] {
		step(c)
	}
	return rem == 1
}

// group is a run of digits inside a candidate number, with what precedes and follows it.
type group struct {
	start, end int  // digits, [start, end)
	open       bool // a '(' directly before the digits
	close      bool // a ')' directly after the digits
	sepAfter   byte // ' ', '-', '.' or 0 when the chain ends here
	plus       bool // a '+' directly before
}

// findNumbers finds payment-card numbers and phone numbers. Digit groups joined by single
// separators form a chain; windows of consecutive groups are tried as cards first (13 to 19
// digits, Luhn, known issuer range, separators only spaces or hyphens), then as phone numbers.
func findNumbers(in string, spans []span) []span {
	for i := 0; i < len(in); i++ {
		if !isDigit(in[i]) {
			continue
		}
		if i > 0 && (isAlnum(in[i-1]) || in[i-1] == '_' || in[i-1] == '-' && i > 1 && isAlnum(in[i-2])) {
			// inside a word, a UUID, an ID or a hash: skip the rest of this run
			for i < len(in) && isAlnum(in[i]) {
				i++
			}
			continue
		}
		if i > 1 && in[i-1] == '+' && (isAlnum(in[i-2]) || in[i-2] == '.') {
			// "python3.14.7+20260901": a build suffix, not an international prefix
			for i < len(in) && isDigit(in[i]) {
				i++
			}
			continue
		}
		groups, next := readChain(in, i)
		spans = scanChain(in, groups, spans)
		i = next - 1
	}
	return spans
}

// readChain reads digit groups from in[i:], joined by one separator each, to at most 8 groups.
// It returns the groups and the index after the chain.
func readChain(in string, i int) ([]group, int) {
	var groups []group
	pos := i
	for len(groups) < 8 {
		g := group{start: pos}
		if pos > 0 {
			switch in[pos-1] {
			case '+':
				g.plus = true
			case '(':
				g.open = true
			}
		}
		for pos < len(in) && isDigit(in[pos]) {
			pos++
		}
		g.end = pos
		if pos < len(in) && in[pos] == ')' {
			g.close = true
			pos++
		}
		if pos < len(in) && (isAlnum(in[pos]) || in[pos] == '_') { // letters glued to digits: not a number
			for pos < len(in) && (isAlnum(in[pos]) || in[pos] == '_') {
				pos++
			}
			groups = append(groups, g)
			groups[len(groups)-1].sepAfter = 'x'
			return groups, pos
		}
		if g.end-g.start > 19 {
			groups = append(groups, g)
			return groups, pos
		}
		// a separator, then a group (possibly "(digits"), continues the chain
		if pos+1 < len(in) && (in[pos] == ' ' || in[pos] == '-' || in[pos] == '.') {
			p := pos + 1
			if in[p] == '(' && p+1 < len(in) {
				p++
			}
			if p < len(in) && isDigit(in[p]) {
				g.sepAfter = in[pos]
				groups = append(groups, g)
				pos = p
				continue
			}
		}
		groups = append(groups, g)
		break
	}
	return groups, pos
}

func scanChain(in string, groups []group, spans []span) []span {
	for a := 0; a < len(groups); {
		bestCard, bestPhone, digits := -1, -1, 0
		for b := a; b < len(groups); b++ {
			if groups[b].sepAfter == 'x' {
				break
			}
			digits += glen(groups[b])
			if digits > 19 {
				break
			}
			if digits >= 13 && cardWindow(in, groups[a:b+1]) {
				bestCard = b
			}
			if digits <= 15 && phoneWindow(in, groups[a:b+1]) {
				bestPhone = b
			}
		}
		best, kind := bestPhone, KindPhone
		if bestCard >= 0 {
			best, kind = bestCard, KindCard
		}
		if best < 0 {
			a++
			continue
		}
		start, end := groups[a].start, groups[best].end
		if groups[a].plus {
			start--
		}
		if groups[a].open {
			start--
		}
		if groups[best].close {
			end++
		}
		spans = append(spans, span{start, end, kind})
		a = best + 1
	}
	return spans
}

// cardWindow reports whether the groups spell a payment-card number.
func cardWindow(in string, gs []group) bool {
	var digits [19]byte
	n := 0
	for i, g := range gs {
		if g.open || g.close || g.plus || (i < len(gs)-1 && gs[i].sepAfter == '.') {
			return false
		}
		for k := g.start; k < g.end; k++ {
			if n == len(digits) {
				return false
			}
			digits[n] = in[k]
			n++
		}
	}
	d := digits[:n]
	return n >= 13 && luhn(d) && cardIssuer(d)
}

func luhn(d []byte) bool {
	sum, double := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		v := int(d[i] - '0')
		if double {
			if v *= 2; v > 9 {
				v -= 9
			}
		}
		sum += v
		double = !double
	}
	return sum%10 == 0
}

// cardIssuer reports whether the number starts like a card network's range and has a length that
// network issues. It keeps a long ID that happens to pass Luhn from being a card.
func cardIssuer(d []byte) bool {
	n := len(d)
	p := func(k int) int {
		v := 0
		for i := 0; i < k && i < n; i++ {
			v = v*10 + int(d[i]-'0')
		}
		return v
	}
	switch {
	case d[0] == '4': // Visa
		return n == 13 || n == 16 || n == 19
	case p(2) >= 51 && p(2) <= 55, p(4) >= 2221 && p(4) <= 2720: // Mastercard
		return n == 16
	case p(2) == 34 || p(2) == 37: // American Express
		return n == 15
	case p(4) == 6011, p(2) == 65, p(3) >= 644 && p(3) <= 649: // Discover
		return n >= 16 && n <= 19
	case p(3) >= 300 && p(3) <= 305, p(2) == 36, p(2) == 38, p(2) == 39: // Diners Club
		return n >= 14 && n <= 19
	case p(4) >= 3528 && p(4) <= 3589: // JCB
		return n >= 16 && n <= 19
	case p(2) == 62: // UnionPay
		return n >= 16 && n <= 19
	case p(2) == 50, p(2) >= 56 && p(2) <= 69: // Maestro and other 5x/6x ranges
		return n >= 12 && n <= 19
	}
	return false
}

// phoneWindow reports whether the groups look like a phone number: 8 to 15 digits (E.164 allows
// 15), written with a leading '+', with parentheses, or in two or more groups joined by spaces or
// hyphens. Dates, dotted versions and IPs, thousands groups ("1 000 000") and equal short groups
// ("100-200-300", "4111 1111 1111") are not phone numbers. A bare run of digits is never one: it is an ID or a
// count.
func phoneWindow(in string, gs []group) bool {
	digits := 0
	paren := false
	dots := false
	for _, g := range gs {
		digits += g.end - g.start
		paren = paren || g.open || g.close
		dots = dots || g.sepAfter == '.'
	}
	plus := gs[0].plus
	for i, g := range gs {
		if g.open != g.close || (g.open && i > 1) { // "(30 days)", "(2026-10-20)": not an area code
			return false
		}
	}
	for i := range gs {
		if yearLike(in, gs[i]) && ((i+1 < len(gs) && glen(gs[i+1]) <= 2 && gs[i].sepAfter == '-') ||
			(i > 0 && glen(gs[i-1]) <= 2 && gs[i-1].sepAfter != ' ')) {
			return false // part of a date
		}
	}
	if digits < 8 || digits > 15 {
		return false
	}
	if !plus && (digits < 9 || dots) {
		return false
	}
	if len(gs) < 2 && !plus {
		return false
	}
	if !plus && !paren {
		if glen(gs[0]) > 5 || len(gs) > 6 || hasShortGroup(gs) || mixedSeparators(gs) {
			return false // a list of numbers, an ID, a table row
		}
		if glen(gs[0]) == 8 && glen(gs[1]) == 4 { // a UUID that is all digits
			return false
		}
		if isDateLike(in, gs) {
			return false
		}
		if thousands(gs) {
			return false
		}
		if len(gs) >= 3 && equalShort(gs) {
			return false
		}
	}
	return true
}

func glen(g group) int { return g.end - g.start }

func isDateLike(in string, gs []group) bool {
	if len(gs) == 3 {
		a, b, c := glen(gs[0]), glen(gs[1]), glen(gs[2])
		if (a == 4 && b <= 2 && c <= 2) || (a <= 2 && b <= 2 && (c == 4 || c == 2)) {
			return true
		}
	}
	return false
}

func thousands(gs []group) bool {
	if glen(gs[0]) > 3 {
		return false
	}
	for _, g := range gs[1:] {
		if glen(g) != 3 {
			return false
		}
	}
	return true
}

func equalShort(gs []group) bool {
	n := glen(gs[0])
	if n > 4 {
		return false
	}
	for _, g := range gs {
		if glen(g) != n {
			return false
		}
	}
	return true
}

func yearLike(in string, g group) bool {
	return glen(g) == 4 && (in[g.start:g.start+2] == "19" || in[g.start:g.start+2] == "20")
}

func hasShortGroup(gs []group) bool {
	for _, g := range gs {
		if glen(g) < 2 {
			return true
		}
	}
	return false
}

func mixedSeparators(gs []group) bool {
	var sep byte
	for _, g := range gs[:len(gs)-1] {
		if sep != 0 && g.sepAfter != sep {
			return true
		}
		sep = g.sepAfter
	}
	return false
}
