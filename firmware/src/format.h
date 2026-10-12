#pragma once
#include <cstddef>
#include "model.h"

// Row text builders. Pure snprintf so they can be unit-tested on the host;
// fitting to pixel width happens in ui.cpp.
void formatRoute(const Nearest& n, char* out, size_t len);
void formatTypeAirline(const Nearest& n, char* out, size_t len);
void formatAltSpeed(const Nearest& n, char* out, size_t len);
void formatDist(const Nearest& n, char* out, size_t len);
void formatRadius(float radiusKm, char* out, size_t len);
void formatDelay(const Nearest& n, char* out, size_t len);  // "" when !hasDelay
