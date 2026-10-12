#pragma once
#include <cstring>
#include "model.h"

// Decides when the panel's logo square needs a fetch or a clear. Pure so
// the host tests can drive it; main.cpp does the I/O it asks for.
//
// Rules: fetch only when the wanted code differs from what is shown (or
// the screen was rebuilt); a 404 is remembered per code for an hour (the
// service caches a miss that long) or until the code goes away; a failed
// fetch for a different airline clears the old logo so it never sits
// beside the wrong callsign.
struct LogoSync {
  enum class Action { None, Clear, Fetch };
  enum class After { Drawn, Cleared, Retry };
  static constexpr unsigned long RETRY_MS = 3600000UL;

  char shown[3] = "";
  char failed[3] = "";
  unsigned long failedAt = 0;

  Action decide(const char* code, bool rebuilt, unsigned long now) {
    if (!code[0]) {
      failed[0] = '\0';
      return clearIfShown(rebuilt);
    }
    if (strcmp(code, failed) == 0) {
      if (now - failedAt < RETRY_MS) return clearIfShown(false);
      failed[0] = '\0';
    }
    if (!rebuilt && strcmp(code, shown) == 0) return Action::None;
    return Action::Fetch;
  }

  After result(FetchResult r, const char* code, unsigned long now) {
    switch (r) {
      case FetchResult::Ok:
        copy(shown, code);
        return After::Drawn;
      case FetchResult::NotFound:
        copy(failed, code);
        failedAt = now;
        shown[0] = '\0';
        return After::Cleared;
      case FetchResult::Error:
      default:
        if (shown[0] && strcmp(shown, code) != 0) {
          shown[0] = '\0';
          return After::Cleared;
        }
        return After::Retry;
    }
  }

  // The screen was rebuilt (clearAll), so nothing is on the panel now.
  void reset() { shown[0] = '\0'; }

 private:
  Action clearIfShown(bool rebuilt) {
    if (!shown[0] && !rebuilt) return Action::None;
    shown[0] = '\0';
    return Action::Clear;
  }
  static void copy(char* dst, const char* src) {
    strncpy(dst, src, 2);
    dst[2] = '\0';
  }
};
