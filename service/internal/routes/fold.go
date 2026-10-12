package routes

import "strings"

// The T-Display's built-in fonts only have ASCII glyphs, and the web mirror
// must show exactly what the display shows, so city names are folded to
// ASCII once here. The table covers Latin-1 Supplement, Latin Extended-A
// and the Romanian comma-below letters. A name with any other non-ASCII
// letter folds to "" as a whole, so the caller shows the code instead of a
// half-dropped name ("Timioara"); combining marks are simply removed.
var foldTable = map[rune]string{}

func init() {
	for base, variants := range map[string]string{
		"A": "ÀÁÂÃÄÅĀĂĄ", "a": "àáâãäåāăą", "AE": "Æ", "ae": "æ",
		"C": "ÇĆĈĊČ", "c": "çćĉċč", "D": "ÐĎĐ", "d": "ðďđ",
		"E": "ÈÉÊËĒĔĖĘĚ", "e": "èéêëēĕėęě", "G": "ĜĞĠĢ", "g": "ĝğġģ",
		"H": "ĤĦ", "h": "ĥħ", "I": "ÌÍÎÏĨĪĬĮİ", "i": "ìíîïĩīĭįı", "IJ": "Ĳ", "ij": "ĳ",
		"J": "Ĵ", "j": "ĵ", "K": "Ķ", "k": "ķĸ", "L": "ĹĻĽĿŁ", "l": "ĺļľŀł",
		"N": "ÑŃŅŇŊ", "n": "ñńņňŉŋ", "O": "ÒÓÔÕÖØŌŎŐ", "o": "òóôõöøōŏő",
		"OE": "Œ", "oe": "œ", "R": "ŔŖŘ", "r": "ŕŗř", "S": "ŚŜŞŠȘ", "s": "śŝşšșſ", "ss": "ß",
		"T": "ŢŤŦȚ", "t": "ţťŧț", "TH": "Þ", "th": "þ",
		"U": "ÙÚÛÜŨŪŬŮŰŲ", "u": "ùúûüũūŭůűų", "W": "Ŵ", "w": "ŵ",
		"Y": "ÝŶŸ", "y": "ýÿŷ", "Z": "ŹŻŽ", "z": "źżž",
	} {
		for _, r := range variants {
			foldTable[r] = base
		}
	}
}

func asciiFold(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r >= 0x300 && r <= 0x36f: // combining diacritical marks
		default:
			folded, ok := foldTable[r]
			if !ok {
				return ""
			}
			b.WriteString(folded)
		}
	}
	return strings.TrimSpace(b.String())
}
