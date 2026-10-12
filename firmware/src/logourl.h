#pragma once
#include <cstddef>
#include <cstdio>
#include <cstring>

// Builds the bitmap URL from the configured /api/nearest URL, so config.h
// keeps a single address. Fails (and logos stay off) when the URL has a
// different shape, rather than fetching from a guessed path.
inline bool logoUrl(const char* nearestUrl, const char* code, char* out, size_t len) {
  static const char suffix[] = "/api/nearest";
  const size_t n = strlen(nearestUrl), s = sizeof suffix - 1;
  if (!code || !code[0] || n < s || strcmp(nearestUrl + n - s, suffix) != 0) return false;
  const int written = snprintf(out, len, "%.*s/api/logo/%s/104x32.rgb565", (int)(n - s), nearestUrl, code);
  return written > 0 && (size_t)written < len;
}
