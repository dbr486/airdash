#include "ui.h"
#include <Arduino.h>
#include "board.h"
#include <cmath>
#include <cstring>
#include "format.h"
#include "battery.h"

namespace {

// A reference, bound at static init: on ttgo it constructs the driver
// object, on sticks3 it only takes M5.Display's address (M5 itself is
// initialised later, in board::begin()).
Gfx& tft = board::display();

// TFT_eSPI names its built-in fonts by number, M5GFX by pointer; both carry
// the same glyphs, so the layout below is identical on either board.
#if defined(AIRDASH_BOARD_STICKS3)
using Font = const lgfx::IFont*;
const Font FONT_SMALL = &fonts::Font2;
const Font FONT_BIG = &fonts::Font4;
#else
using Font = int;
constexpr Font FONT_SMALL = 2;
constexpr Font FONT_BIG = 4;
#endif

constexpr int W = 240;
constexpr int ROW_Y[5] = {2, 34, 56, 78, 100};
constexpr int ROW_H[5] = {30, 18, 18, 18, 18};
constexpr int LEFT_OF_LOGO_W = 130;   // the callsign row stops 4 px short of the logo (font metrics are not exact)
constexpr int ROW5_MAX_W = 170;   // leaves room for the battery icon and the bearing triangle
// A landscape box beside the callsign: wordmarks are wide, so the service
// crops the CDN's square padding and fits the mark into 104x32. It ends
// above the route row (y=34), which keeps its full width.
constexpr int LOGO_X = 134, LOGO_Y = 1, LOGO_W = 104, LOGO_H = 32;
constexpr int MARK_Y = 119;
constexpr int TRI_CX = 222, TRI_CY = 106, TRI_R = 10;
// Battery icon on the distance/counter row, just left of the bearing
// triangle and centred on it, so the marks line stays free.
constexpr int BAT_X = 185, BAT_Y = 101, BAT_W = 20, BAT_H = 10;
constexpr int PLUS_X = 178, PLUS_Y = 106;   // centre of the charging "+"

enum class Screen { None, Boot, Aircraft, NoTraffic };
Screen screen = Screen::None;

// Last drawn text per row, so an unchanged row is not repainted (no flicker).
char lastRow[5][48];
char lastStatus[5][12];
int lastBearing = -1;
bool lastStale = false, lastErr = false;
bool dotOn = false;
bool backlightOn = true;
int batPercent = -1;
bool batCharging = false;
int lastBars = -2;   // -2 = nothing drawn yet; -1 = icon hidden
BatteryBand lastBand = BatteryBand::High;
bool lastCharging = false;

void resetRowCache() {
  for (auto& r : lastRow) r[0] = '\0';
  for (auto& st : lastStatus) st[0] = '\0';
  lastBearing = -1;
}

// Drops trailing characters until the text fits maxW pixels in font.
void fit(char* s, int maxW, Font font) {
  size_t n = strlen(s);
  while (n > 0 && tft.textWidth(s, font) > maxW) s[--n] = '\0';
}

void drawRow(int i, const char* text, uint16_t color, int maxW) {
  char buf[48];
  strncpy(buf, text, sizeof buf - 1);
  buf[sizeof buf - 1] = '\0';
  const Font font = (i == 0) ? FONT_BIG : FONT_SMALL;
  fit(buf, maxW, font);
  if (strcmp(buf, lastRow[i]) == 0) return;
  tft.fillRect(0, ROW_Y[i], maxW, ROW_H[i], TFT_BLACK);
  tft.setTextColor(color, TFT_BLACK);
  tft.drawString(buf, 2, ROW_Y[i], font);
  strcpy(lastRow[i], buf);
}

uint16_t delayColor(const char* kind) {
  if (strcmp(kind, "late") == 0) return TFT_YELLOW;
  if (strcmp(kind, "early") == 0) return TFT_GREEN;
  if (strcmp(kind, "cancelled") == 0 || strcmp(kind, "diverted") == 0) return TFT_RED;
  return TFT_WHITE;
}

// A row with two parts: text on the left and a status right-aligned to
// rightEdge. Either changing repaints the whole row (up to rightEdge) so a
// vanished status leaves no residue and the text regains its width.
void drawSplitRow(int i, const char* text, uint16_t color, const char* status, uint16_t statusColor, int rightEdge) {
  char buf[48];
  strncpy(buf, text, sizeof buf - 1);
  buf[sizeof buf - 1] = '\0';
  const int statusW = status[0] ? tft.textWidth(status, FONT_SMALL) : 0;
  const int textMax = status[0] ? (rightEdge - statusW - 6) : rightEdge - 2;
  fit(buf, textMax, FONT_SMALL);
  if (strcmp(buf, lastRow[i]) == 0 && strcmp(status, lastStatus[i]) == 0) return;
  tft.fillRect(0, ROW_Y[i], rightEdge, ROW_H[i], TFT_BLACK);
  tft.setTextColor(color, TFT_BLACK);
  tft.drawString(buf, 2, ROW_Y[i], FONT_SMALL);
  if (status[0]) {
    tft.setTextColor(statusColor, TFT_BLACK);
    tft.drawString(status, rightEdge - statusW, ROW_Y[i], FONT_SMALL);
  }
  strcpy(lastRow[i], buf);
  strncpy(lastStatus[i], status, sizeof lastStatus[i] - 1);
  lastStatus[i][sizeof lastStatus[i] - 1] = '\0';
}

void drawBearing(int bearing) {
  if (bearing == lastBearing) return;
  tft.fillRect(TRI_CX - TRI_R - 1, TRI_CY - TRI_R - 1, 2 * TRI_R + 3, 2 * TRI_R + 3, TFT_BLACK);
  const float a = bearing * (float)M_PI / 180.0f;   // 0 = up (north)
  auto px = [&](float ang, float r) { return (int)lroundf(TRI_CX + r * sinf(ang)); };
  auto py = [&](float ang, float r) { return (int)lroundf(TRI_CY - r * cosf(ang)); };
  const float back = 140.0f * (float)M_PI / 180.0f;
  tft.fillTriangle(px(a, TRI_R), py(a, TRI_R),
                   px(a + back, TRI_R * 0.7f), py(a + back, TRI_R * 0.7f),
                   px(a - back, TRI_R * 0.7f), py(a - back, TRI_R * 0.7f),
                   TFT_WHITE);
  lastBearing = bearing;
}

void drawBattery() {
  const int bars = batteryBars(batPercent);
  const BatteryBand band = batteryBand(batPercent);
  if (bars == lastBars && band == lastBand && batCharging == lastCharging) return;
  tft.fillRect(PLUS_X - 3, BAT_Y, BAT_X + BAT_W + 2 - (PLUS_X - 3), BAT_H, TFT_BLACK);
  lastBars = bars;
  lastBand = band;
  lastCharging = batCharging;
  if (bars < 0) return;
  const uint16_t c = band == BatteryBand::Low ? TFT_RED : band == BatteryBand::Mid ? TFT_YELLOW : TFT_GREEN;
  tft.drawRect(BAT_X, BAT_Y, BAT_W, BAT_H, band == BatteryBand::Low ? TFT_RED : TFT_WHITE);
  tft.fillRect(BAT_X + BAT_W, BAT_Y + 3, 2, BAT_H - 6, TFT_WHITE);   // terminal nub
  for (int i = 0; i < bars; i++) tft.fillRect(BAT_X + 2 + i * 4, BAT_Y + 2, 3, BAT_H - 4, c);
  if (batCharging) {
    tft.drawFastHLine(PLUS_X - 2, PLUS_Y, 5, TFT_WHITE);
    tft.drawFastVLine(PLUS_X, PLUS_Y - 2, 5, TFT_WHITE);
  }
}

void clearAll() {
  tft.fillScreen(TFT_BLACK);
  resetRowCache();
  lastStale = lastErr = false;
  lastBars = -2;   // the wipe took the icon with it
  drawBattery();
}

void drawMarks() {
  tft.fillRect(0, MARK_Y, 40, 16, TFT_BLACK);
  if (lastStale) {
    tft.setTextColor(TFT_YELLOW, TFT_BLACK);
    tft.drawString("stale", 2, MARK_Y, FONT_SMALL);
  }
  tft.fillRect(W - 12, MARK_Y, 12, 16, TFT_BLACK);
  if (lastErr) {
    tft.setTextColor(TFT_RED, TFT_BLACK);
    tft.drawString("x", 228, MARK_Y, FONT_SMALL);
  }
}

}  // namespace

namespace ui {

void begin() {
  board::begin();
  clearAll();
}

void showBoot(const char* ssid) {
  clearAll();
  screen = Screen::Boot;
  tft.setTextColor(TFT_WHITE, TFT_BLACK);
  tft.drawString("AirDash", 2, 2, FONT_BIG);
  char line[48];
  snprintf(line, sizeof line, "WiFi %s ...", ssid);
  showBootStatus(line);
}

void showBootStatus(const char* line) {
  if (screen != Screen::Boot) return;
  tft.fillRect(0, 34, W, 18, TFT_BLACK);
  tft.setTextColor(TFT_WHITE, TFT_BLACK);
  tft.drawString(line, 2, 34, FONT_SMALL);
}

void showAircraft(const Nearest& n) {
  if (screen != Screen::Aircraft) {
    clearAll();
    screen = Screen::Aircraft;
  }
  char buf[48];
  drawRow(0, n.callsign, TFT_WHITE, LEFT_OF_LOGO_W);
  formatRoute(n, buf, sizeof buf);
  drawSplitRow(1, buf, TFT_CYAN, "", TFT_WHITE, 238);
  char status[12];
  formatDelay(n, status, sizeof status);
  formatTypeAirline(n, buf, sizeof buf);
  drawSplitRow(2, buf, TFT_WHITE, status, delayColor(n.delayKind), 238);
  formatAltSpeed(n, buf, sizeof buf);
  drawRow(3, buf, TFT_WHITE, W);
  formatDist(n, buf, sizeof buf);
  drawRow(4, buf, TFT_WHITE, ROW5_MAX_W);
  drawBearing(n.bearing);
  if (lastStale || lastErr) drawMarks();   // marks sit below the rows; keep them
}

void showNoTraffic(const Nearest& n) {
  if (screen != Screen::NoTraffic) {
    clearAll();
    screen = Screen::NoTraffic;
    tft.setTextColor(TFT_WHITE, TFT_BLACK);
    tft.drawString("No aircraft", 2, 30, FONT_BIG);
  }
  char buf[32];
  formatRadius(n.radiusKm, buf, sizeof buf);
  if (strcmp(buf, lastRow[1]) != 0) {
    tft.fillRect(0, 66, W, 18, TFT_BLACK);
    tft.setTextColor(TFT_WHITE, TFT_BLACK);
    tft.drawString(buf, 2, 66, FONT_SMALL);
    strcpy(lastRow[1], buf);
  }
}

void toggleAliveDot() {
  dotOn = !dotOn;
  tft.fillCircle(232, 6, 3, dotOn ? TFT_WHITE : TFT_BLACK);
}

void setMarks(bool stale, bool err) {
  if (stale == lastStale && err == lastErr) return;
  lastStale = stale;
  lastErr = err;
  drawMarks();
}

void setBattery(int percent, bool charging) {
  batPercent = percent;
  batCharging = charging;
  drawBattery();
}

void toggleBacklight() {
  backlightOn = !backlightOn;
  board::setBacklight(backlightOn);
}

void showLogo(const uint8_t* rgb565) {
  board::pushRgb565BE(LOGO_X, LOGO_Y, LOGO_W, LOGO_H, rgb565);
}

void clearLogo() {
  tft.fillRect(LOGO_X, LOGO_Y, LOGO_W, LOGO_H, TFT_BLACK);
}

}  // namespace ui
