#pragma once

// Edge-detecting button with a bounce lockout: fires once per press,
// however long the button is held. Pure so it can be unit-tested on the
// host; the caller feeds it the pin level and millis().
enum class Press { None, Short, Long };

struct Button {
  bool wasDown = false;
  unsigned long lastEdgeAt = 0;
  bool holdDown = false;
  bool longFired = false;
  unsigned long holdStart = 0;

  // Short/long classifier for a button that carries two functions: Long
  // fires once while the button is still held (so the user gets feedback
  // without releasing), Short fires on a release that happened before the
  // long threshold. Bounce inside lockoutMs is ignored like in pressed().
  Press hold(bool levelLow, unsigned long now, unsigned long longMs = 700, unsigned long lockoutMs = 200) {
    if (levelLow) {
      if (!holdDown) {
        if (now - lastEdgeAt < lockoutMs) return Press::None;
        holdDown = true;
        longFired = false;
        holdStart = now;
        lastEdgeAt = now;
        return Press::None;
      }
      if (!longFired && now - holdStart >= longMs) {
        longFired = true;
        return Press::Long;
      }
      return Press::None;
    }
    if (!holdDown) return Press::None;
    if (now - lastEdgeAt < lockoutMs) return Press::None;   // bounce, still pressed
    holdDown = false;
    lastEdgeAt = now;
    if (longFired) return Press::None;
    // The loop may have been blocked (a poll or logo fetch) for the whole
    // hold, so the release can be the first sample past the threshold.
    return now - holdStart >= longMs ? Press::Long : Press::Short;
  }

  // levelLow: the pin currently reads LOW (pressed). Returns true only on
  // the release-to-press edge, and not within lockoutMs of the previous
  // edge (contact bounce).
  bool pressed(bool levelLow, unsigned long now, unsigned long lockoutMs = 200) {
    if (!levelLow) {
      // A high level inside the lockout is contact bounce, not a release:
      // keep the press latched so it cannot fire again when the pin
      // settles low.
      if (now - lastEdgeAt >= lockoutMs) wasDown = false;
      return false;
    }
    if (wasDown) return false;
    if (now - lastEdgeAt < lockoutMs) return false;
    wasDown = true;
    lastEdgeAt = now;
    return true;
  }
};
