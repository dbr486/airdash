#include "parse.h"
#include <ArduinoJson.h>
#include <cstring>

static void copyStr(char* dst, size_t len, const char* src) {
  if (!src) src = "";
  strncpy(dst, src, len - 1);
  dst[len - 1] = '\0';
}

static int clampInt(int v, int lo, int hi) {
  if (v < lo) return lo;
  if (v > hi) return hi;
  return v;
}

bool parseNearest(const char* json, Nearest& out) {
  if (!json) return false;
  // 1536: fits the worst-case body (max-length strings on every field, plus
  // the optional delay object) with headroom; 768 ran out of room on
  // that exact body on the live T-Display.
  StaticJsonDocument<1536> doc;
  if (deserializeJson(doc, json) != DeserializationError::Ok) return false;
  if (!doc.containsKey("found")) return false;

  Nearest n;
  n.found = doc["found"] | false;
  n.ageS = doc["age_s"] | 0;
  n.nearby = doc["nearby"] | 0;
  n.radiusKm = doc["radius_km"] | 0.0f;
  if (n.found) {
    copyStr(n.callsign, sizeof n.callsign, doc["callsign"] | "");
    copyStr(n.hex, sizeof n.hex, doc["hex"] | "");
    n.index = clampInt(doc["index"] | 0, 0, 9999);
    copyStr(n.type, sizeof n.type, doc["type"] | "");
    copyStr(n.reg, sizeof n.reg, doc["reg"] | "");
    copyStr(n.airline, sizeof n.airline, doc["airline"] | "");
    copyStr(n.origin, sizeof n.origin, doc["origin"] | "");
    copyStr(n.dest, sizeof n.dest, doc["dest"] | "");
    copyStr(n.originLabel, sizeof n.originLabel, doc["origin_label"] | (const char*)n.origin);
    copyStr(n.destLabel, sizeof n.destLabel, doc["dest_label"] | (const char*)n.dest);
    copyStr(n.logo, sizeof n.logo, doc["logo"] | "");
    n.altFt = doc["alt_ft"] | 0;
    n.gsKt = doc["gs_kt"] | 0;
    n.track = doc["track"] | 0;
    n.distKm = doc["dist_km"] | 0.0f;
    n.bearing = doc["bearing"] | 0;
    if (doc["delay"].is<JsonObjectConst>()) {
      const char* kind = doc["delay"]["kind"] | "";
      static const char* known[] = {"on_time", "late", "early", "cancelled", "diverted"};
      for (const char* k : known) {
        if (strcmp(kind, k) == 0) {
          n.hasDelay = true;
          copyStr(n.delayKind, sizeof n.delayKind, kind);
          n.delayMin = clampInt(doc["delay"]["min"] | 0, -9999, 9999);
          break;
        }
      }
    }
  }
  out = n;
  return true;
}
