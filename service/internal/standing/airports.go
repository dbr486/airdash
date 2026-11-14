package standing

import (
	"bytes"
	_ "embed"
	"encoding/csv"
	"io"
	"strings"
	"sync"
)

// data/airports.csv is a filtered OurAirports extract (iata,icao,city) made
// by scripts/update-standing-data.sh. Some route sources (hexdb, the AirLabs
// board) send airport codes only; this names their ends without a network
// call.
//
//go:embed data/airports.csv
var airportsCSV []byte

type airportIndex struct{ byIATA, byICAO map[string]string }

// The two codes live in separate maps: a short ICAO ident such as "03N"
// must never answer an IATA lookup.
var airportCities = sync.OnceValue(func() airportIndex {
	idx := airportIndex{byIATA: map[string]string{}, byICAO: map[string]string{}}
	r := csv.NewReader(bytes.NewReader(airportsCSV))
	r.FieldsPerRecord = -1
	for first := true; ; first = false {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || first || len(rec) < 3 {
			continue
		}
		city := cityName(rec[2])
		if city == "" {
			continue
		}
		if iata := strings.ToUpper(strings.TrimSpace(rec[0])); iata != "" {
			if _, dup := idx.byIATA[iata]; !dup {
				idx.byIATA[iata] = city
			}
		}
		if icao := strings.ToUpper(strings.TrimSpace(rec[1])); icao != "" {
			if _, dup := idx.byICAO[icao]; !dup {
				idx.byICAO[icao] = city
			}
		}
	}
	return idx
})

// cityName keeps the part before the first comma: OurAirports writes
// "Manchester, Greater Manchester", which is too long for the display row.
func cityName(municipality string) string {
	city, _, _ := strings.Cut(municipality, ",")
	return asciiFold(city)
}

// AirportCity names an airport by its IATA code, else its ICAO code, or
// returns "" when neither is known (the display then shows the code).
func AirportCity(iata, icao string) string {
	idx := airportCities()
	if city, ok := idx.byIATA[strings.ToUpper(strings.TrimSpace(iata))]; ok {
		return city
	}
	return idx.byICAO[strings.ToUpper(strings.TrimSpace(icao))]
}
