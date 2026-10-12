#include "format.h"
#include <cstdio>
#include <cmath>
#include <cstring>

void formatRoute(const Nearest& n, char* out, size_t len) {
  const bool hasO = n.originLabel[0] != '\0';
  const bool hasD = n.destLabel[0] != '\0';
  if (hasO || hasD) {
    snprintf(out, len, "%s -> %s", hasO ? n.originLabel : "?", hasD ? n.destLabel : "?");
  } else if (n.airline[0]) {
    snprintf(out, len, "%s", n.airline);
  } else {
    snprintf(out, len, "%s", n.reg);
  }
}

void formatTypeAirline(const Nearest& n, char* out, size_t len) {
  if (n.type[0] && n.airline[0]) {
    snprintf(out, len, "%s  %s", n.type, n.airline);
  } else {
    snprintf(out, len, "%s", n.type[0] ? n.type : n.airline);
  }
}

void formatAltSpeed(const Nearest& n, char* out, size_t len) {
  snprintf(out, len, "%d ft  %d kt", n.altFt, n.gsKt);
}

void formatDist(const Nearest& n, char* out, size_t len) {
  if (n.nearby > 1) {
    snprintf(out, len, "%.1f km  %d/%d", (double)n.distKm, n.index + 1, n.nearby);
  } else {
    snprintf(out, len, "%.1f km", (double)n.distKm);
  }
}

void formatRadius(float radiusKm, char* out, size_t len) {
  if (fabsf(radiusKm - roundf(radiusKm)) < 0.05f) {
    snprintf(out, len, "within %d km", (int)roundf(radiusKm));
  } else {
    snprintf(out, len, "within %.1f km", (double)radiusKm);
  }
}

void formatDelay(const Nearest& n, char* out, size_t len) {
  if (!n.hasDelay) { out[0] = '\0'; return; }
  if (strcmp(n.delayKind, "late") == 0)       snprintf(out, len, "Late %dm", n.delayMin);
  else if (strcmp(n.delayKind, "early") == 0) snprintf(out, len, "Early %dm", n.delayMin < 0 ? -n.delayMin : n.delayMin);
  else if (strcmp(n.delayKind, "on_time") == 0)   snprintf(out, len, "On time");
  else if (strcmp(n.delayKind, "cancelled") == 0) snprintf(out, len, "Cancelled");
  else if (strcmp(n.delayKind, "diverted") == 0)  snprintf(out, len, "Diverted");
  else out[0] = '\0';
}
