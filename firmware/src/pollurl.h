#pragma once
#include <cstddef>
#include <cstdio>

// Appends ?param=value to the configured poll URL. An empty param means
// the plain nearest request. Fails on truncation so a half-built URL is
// never fetched.
inline bool pollUrl(const char* base, const char* param, const char* value, char* out, size_t len) {
  const int n = param[0] ? snprintf(out, len, "%s?%s=%s", base, param, value) : snprintf(out, len, "%s", base);
  return n > 0 && (size_t)n < len;
}
