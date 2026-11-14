package standing

import "strings"

// typeNames maps ICAO type designators to names short enough for the small
// display (14 characters max, enforced by a test).
var typeNames = map[string]string{
	"A319": "A319", "A320": "A320", "A321": "A321", "A20N": "A320neo", "A21N": "A321neo",
	"A332": "A330-200", "A333": "A330-300", "A339": "A330-900", "A359": "A350-900",
	"B737": "737-700", "B738": "737-800", "B739": "737-900", "B38M": "737 MAX 8", "B39M": "737 MAX 9",
	"B752": "757-200", "B763": "767-300", "B772": "777-200", "B77W": "777-300ER",
	"B788": "787-8", "B789": "787-9",
	"E190": "Embraer 190", "E195": "Embraer 195", "E290": "E190-E2", "E295": "E195-E2",
	"AT75": "ATR 72-500", "AT76": "ATR 72-600", "CRJ9": "CRJ-900", "DH8D": "Dash 8-400",
	"C56X": "Citation XLS", "C68A": "Latitude", "C25A": "CitationJet 2", "C25B": "CitationJet 3",
	"C25C": "CitationJet 4", "CL35": "Challenger 350", "CL60": "Challenger 600",
	"GLF5": "Gulfstream V", "GLF6": "Gulfstream 650", "GLEX": "Global Express",
	"PC12": "Pilatus PC-12", "E55P": "Phenom 300", "E50P": "Phenom 100",
	"F2TH": "Falcon 2000", "FA7X": "Falcon 7X", "FA8X": "Falcon 8X",
	"LJ45": "Learjet 45", "PC24": "Pilatus PC-24", "BE20": "King Air 200",
}

// TypeName returns a short readable name for an ICAO type designator, or ""
// when the type is not in the table so callers can fall back to the code.
func TypeName(icao string) string {
	return typeNames[strings.ToUpper(strings.TrimSpace(icao))]
}
