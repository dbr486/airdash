#pragma once
#include "model.h"

// Decodes one /api/nearest body into out. Returns false when the body is
// not JSON or lacks the "found" key; out is untouched in that case.
bool parseNearest(const char* json, Nearest& out);
