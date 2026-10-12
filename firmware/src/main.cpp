#include <Arduino.h>
#include "config.h"
#include "model.h"
#include "net.h"
#include "ui.h"
#include "board.h"
#include "buttons.h"
#include "reconnect.h"
#include "stale.h"
#include "logourl.h"
#include "logosync.h"
#include "pollurl.h"
#include <cstring>

static constexpr int STALE_AFTER_S = 30;
static constexpr unsigned long BATTERY_MS = 30000;   // the gauge moves slowly; I2C reads are not free

static bool haveData = false;      // at least one Ok poll has been rendered
static char selectedHex[8] = "";   // aircraft being followed ("" = nearest)
static bool nextRequested = false; // next poll asks for the aircraft after the selection
alignas(4) static uint8_t logoBuf[ui::LOGO_BYTES];   // pushed as uint16_t pairs
static LogoSync logoSync;
static bool screenWasAircraft = false;
static bool staleFlag = false;     // cached result of Staleness::update
static Staleness staleness;
static bool wifiWasUp = false;
static unsigned long nextPollAt = 0;
static unsigned long nextBatteryAt = 0;
static Button btnRefresh, btnNext;
static Reconnector reconnector;

static void logWifiUp() {
  char ip[16];
  net::localIp(ip, sizeof ip);
  Serial.printf("wifi up %s\n", ip);
  ui::showBootStatus(ip);   // no-op once a data screen is showing
}

// LogoSync decides; this does the fetch and the drawing it asks for.
static void syncLogo(const Nearest& n, bool screenRebuilt, unsigned long now) {
  switch (logoSync.decide(n.logo, screenRebuilt, now)) {
    case LogoSync::Action::None:
      return;
    case LogoSync::Action::Clear:
      ui::clearLogo();
      Serial.println("logo cleared");
      return;
    case LogoSync::Action::Fetch:
      break;
  }
  char url[96];
  if (!logoUrl(AIRDASH_URL, n.logo, url, sizeof url)) return;
  const FetchResult r = net::fetchLogo(url, logoBuf, sizeof logoBuf);
  switch (logoSync.result(r, n.logo, now)) {
    case LogoSync::After::Drawn:
      ui::showLogo(logoBuf);
      Serial.printf("logo %s drawn\n", n.logo);
      break;
    case LogoSync::After::Cleared:
      ui::clearLogo();
      Serial.printf("logo %s %s\n", n.logo, r == FetchResult::NotFound ? "none" : "fetch-error");
      break;
    case LogoSync::After::Retry:
      Serial.printf("logo %s fetch-error\n", n.logo);   // retried on the next poll
      break;
  }
}

static void doPoll(unsigned long now) {
  Nearest n;
  char url[160];
  const char* param = nextRequested ? "after" : (selectedHex[0] ? "hex" : "");
  nextRequested = false;
  if (!pollUrl(AIRDASH_URL, param, selectedHex, url, sizeof url)) {
    strncpy(url, AIRDASH_URL, sizeof url - 1);
    url[sizeof url - 1] = '\0';
    selectedHex[0] = '\0';
  }
  const PollResult r = net::poll(url, n);
  const bool ok = r == PollResult::Ok;
  staleFlag = staleness.update(ok, r == PollResult::ParseError, n.ageS, now, STALE_AFTER_S);
  switch (r) {
    case PollResult::Ok:
      haveData = true;
      if (n.found) {
        ui::showAircraft(n);
        syncLogo(n, !screenWasAircraft, now);
        screenWasAircraft = true;
        // Keep following the aircraft we asked for even while it happens to
        // be the nearest; a different hex at index 0 means the selection
        // left range (or a step wrapped back to the nearest), so drop it.
        if (n.index > 0 || strcmp(n.hex, selectedHex) == 0) { strncpy(selectedHex, n.hex, sizeof selectedHex - 1); selectedHex[sizeof selectedHex - 1] = '\0'; }
        else selectedHex[0] = '\0';
        Serial.printf("poll ok found=1 cs=%s dist=%.1f idx=%d/%d\n", n.callsign, (double)n.distKm, n.index + 1, n.nearby);
      } else {
        ui::showNoTraffic(n);
        screenWasAircraft = false;
        selectedHex[0] = '\0';
        logoSync.reset();
        ui::toggleAliveDot();
        Serial.printf("poll ok found=0 nearby=%d\n", n.nearby);
      }
      ui::setMarks(staleFlag, false);
      break;
    case PollResult::ParseError:
      ui::setMarks(staleFlag, false);
      break;
    case PollResult::WifiDown:
      Serial.println("poll wifi-down");
      selectedHex[0] = '\0';
      ui::setMarks(staleFlag, true);
      break;
    case PollResult::HttpError:
      selectedHex[0] = '\0';
      ui::setMarks(staleFlag, true);
      break;
  }
}

void setup() {
  Serial.begin(115200);
  delay(200);
  Serial.println("airdash boot");

  ui::begin();
  ui::showBoot(WIFI_SSID);
  net::begin(WIFI_SSID, WIFI_PASS);
  if (net::waitForWifi(20000)) {
    wifiWasUp = true;
    logWifiUp();
    delay(1000);
  } else {
    Serial.println("wifi timeout");
    ui::showBootStatus("WiFi failed, retrying");
  }
  nextPollAt = millis();
}

void loop() {
  const unsigned long now = millis();
  board::update();

  const bool up = net::connected();
  if (up != wifiWasUp) {
    wifiWasUp = up;
    if (up) {
      Serial.println("wifi reconnected");
      logWifiUp();
    } else {
      Serial.println("wifi lost");
      ui::setMarks(staleFlag, true);
    }
  }

  if (reconnector.due(up, now)) net::retry();

  if ((long)(now - nextBatteryAt) >= 0) {
    nextBatteryAt = now + BATTERY_MS;
    const int pct = board::batteryPercent();
    const bool chg = board::charging();
    ui::setBattery(pct, chg);
    if (pct >= 0) Serial.printf("battery %d%%%s\n", pct, chg ? " charging" : "");
  }

  if (btnRefresh.pressed(board::refreshDown(), now)) {
    nextPollAt = now;
    Serial.println("btn: refresh");
  }
  switch (btnNext.hold(board::nextDown(), now)) {
    case Press::Short:
      nextRequested = true;
      nextPollAt = now;
      Serial.println("btn: next");
      break;
    case Press::Long:
      ui::toggleBacklight();
      Serial.println("btn: backlight");
      break;
    case Press::None:
      break;
  }

  if ((long)(now - nextPollAt) >= 0) {
    nextPollAt = now + POLL_MS;
    if (up) {
      doPoll(now);
    } else {
      // No poll while the link is down, but the screen still ages. A
      // selection or a queued "next" from before the outage is stale too.
      selectedHex[0] = '\0';
      nextRequested = false;
      staleFlag = staleness.update(false, false, 0, now, STALE_AFTER_S);
      if (haveData) ui::setMarks(staleFlag, true);
      else ui::showBootStatus("WiFi failed, retrying");
    }
  }
  delay(10);
}
