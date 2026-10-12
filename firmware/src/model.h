#pragma once
#include <cstddef>

// One decoded /api/nearest response. Fixed buffers so the poll path never
// allocates; parse.cpp truncates and NUL-terminates every string.
struct Nearest {
  bool found = false;
  char callsign[12] = "";
  // ICAO hex as the feed sends it (may carry a ~ prefix); the client's
  // selection key when stepping through the aircraft in range.
  char hex[8] = "";
  char type[8] = "";
  char reg[12] = "";
  char airline[32] = "";
  char origin[5] = "";
  char dest[5] = "";
  // What the route row renders: the service resolves city names or codes
  // from its show_names setting; we only fall back to the codes when an
  // older service omits the labels.
  char originLabel[24] = "";
  char destLabel[24] = "";
  // Airline code whose logo to draw (service-resolved from show_logos),
  // "" for none. Two characters plus NUL.
  char logo[3] = "";
  int altFt = 0;
  int gsKt = 0;
  int track = 0;
  float distKm = 0;
  int bearing = 0;
  int ageS = 0;
  int nearby = 0;
  int index = 0;   // position in the in-range list, 0 = nearest
  float radiusKm = 0;
  // Optional delay status from AeroAPI. hasDelay is false when
  // the service omitted the object or sent a kind this firmware doesn't know.
  bool hasDelay = false;
  char delayKind[10] = "";
  int delayMin = 0;
};

enum class PollResult { Ok, WifiDown, HttpError, ParseError };
enum class FetchResult { Ok, NotFound, Error };
