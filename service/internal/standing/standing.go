// Package standing holds reference data that ships inside the binary, so
// naming an operator or aircraft type costs no network call and no API quota.
package standing

import (
	"bytes"
	_ "embed"
	"encoding/csv"
	"io"
	"regexp"
	"strings"
	"sync"
)

// Airline is the display form of one VRS airline row.
type Airline struct {
	Name string
	IATA string
}

//go:embed data/airlines.csv
var airlinesCSV []byte

var iataPattern = regexp.MustCompile(`^[A-Z0-9]{2}$`)

// Airlines are indexed once on first use; the list is static for the life of
// the process, so there is no refresh path.
var airlines = sync.OnceValue(func() map[string]Airline {
	// The upstream file starts with a UTF-8 BOM that would otherwise glue
	// itself to the first header name.
	data := bytes.TrimPrefix(airlinesCSV, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(data))
	r.LazyQuotes = true
	r.FieldsPerRecord = -1

	index := map[string]Airline{}
	cols := map[string]int{}
	for first := true; ; first = false {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue // one malformed row must not drop the rest of the list
		}
		if first {
			for i, h := range rec {
				cols[h] = i
			}
			continue
		}
		get := func(name string) string {
			if i, ok := cols[name]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		icao := strings.ToUpper(get("ICAO"))
		if icao == "" {
			continue
		}
		if _, dup := index[icao]; dup {
			continue // first row per ICAO wins
		}
		iata := strings.ToUpper(get("IATA"))
		if !iataPattern.MatchString(iata) {
			iata = "" // the file holds placeholders like "N/A" for no code
		}
		index[icao] = Airline{Name: asciiFold(get("Name")), IATA: iata}
	}
	return index
})

// LookupAirline finds an operator by its ICAO airline designator.
func LookupAirline(icao string) (Airline, bool) {
	a, ok := airlines()[strings.ToUpper(strings.TrimSpace(icao))]
	if !ok || a.Name == "" {
		return Airline{}, false
	}
	return a, true
}

// CallsignPrefix returns the ICAO airline designator of a commercial-style
// callsign: three letters then a digit ("EXS6M"). Registrations and military
// callsigns ("N123AB", "ECMKO", "D-CAFE") do not have that shape, and
// treating them as airlines would label private aircraft with a wrong
// operator.
func CallsignPrefix(callsign string) (string, bool) {
	cs := strings.ToUpper(strings.TrimSpace(callsign))
	if len(cs) < 4 {
		return "", false
	}
	for i := 0; i < 3; i++ {
		if cs[i] < 'A' || cs[i] > 'Z' {
			return "", false
		}
	}
	if cs[3] < '0' || cs[3] > '9' {
		return "", false
	}
	return cs[:3], true
}

// The display fonts only have ASCII glyphs, so names are folded once at load
// time. A name with any letter outside the table folds to "" and is dropped
// as a whole rather than shown half-eaten. Same rule as the routes package,
// copied so this package stays dependency-free.
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
