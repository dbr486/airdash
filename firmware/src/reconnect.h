#pragma once

// Schedules explicit WiFi reconnect attempts while the link is down. The
// core's auto-reconnect gives up for some disconnect reasons (e.g. an
// auth failure during a router reboot), so the loop retries every
// intervalMs after a grace period that lets auto-reconnect go first.
struct Reconnector {
  bool wasConnected = true;
  unsigned long lastAttemptAt = 0;

  bool due(bool connected, unsigned long now, unsigned long intervalMs = 10000) {
    if (connected) {
      wasConnected = true;
      return false;
    }
    if (wasConnected) {          // just dropped: start the grace period
      wasConnected = false;
      lastAttemptAt = now;
      return false;
    }
    if (now - lastAttemptAt < intervalMs) return false;
    lastAttemptAt = now;
    return true;
  }
};
