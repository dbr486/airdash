#pragma once

// Decides the "stale" mark. Data is stale when the service itself reports
// it old (age_s), when the last body failed to parse, or when nothing has
// been rendered successfully for staleAfterS — so an outage ages the
// screen even though no age_s arrives during it. Before the first render
// there is nothing on screen to be stale.
struct Staleness {
  unsigned long lastOkAt = 0;
  bool haveOk = false;
  bool parseFailed = false;

  bool update(bool ok, bool parseError, int ageS, unsigned long now, int staleAfterS = 30) {
    if (ok) {
      lastOkAt = now;
      haveOk = true;
      parseFailed = false;
      return ageS > staleAfterS;
    }
    if (parseError) parseFailed = true;
    if (parseFailed) return true;
    return haveOk && (now - lastOkAt) > (unsigned long)staleAfterS * 1000UL;
  }
};
