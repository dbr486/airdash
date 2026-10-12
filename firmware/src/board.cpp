#include "board.h"
#include <Arduino.h>

#if defined(AIRDASH_BOARD_TTGO)

namespace {
// GPIO 35 is input-only with a pull-up on the board; GPIO 0 needs the
// internal pull-up. Both buttons read LOW when pressed. They sit above and
// below the USB port, so they are named by function, not side.
constexpr int BTN_REFRESH = 35;
constexpr int BTN_NEXT = 0;   // short press = next aircraft, hold = backlight
}

namespace board {
// Function-local so the driver exists before anything (ui.cpp binds a
// reference at static init) can reach it, whatever the link order.
Gfx& display() {
  static TFT_eSPI tft;
  return tft;
}

void begin() {
  pinMode(BTN_REFRESH, INPUT);
  pinMode(BTN_NEXT, INPUT_PULLUP);
  Gfx& tft = display();
  tft.init();
  tft.setRotation(1);
  pinMode(TFT_BL, OUTPUT);
  digitalWrite(TFT_BL, HIGH);
}

void update() {}

void setBacklight(bool on) { digitalWrite(TFT_BL, on ? HIGH : LOW); }

// Big-endian RGB565 is the panel's wire order, so the bytes go out
// untouched (setSwapBytes stays false).
void pushRgb565BE(int x, int y, int w, int h, const uint8_t* data) {
  display().pushImage(x, y, w, h, reinterpret_cast<const uint16_t*>(data));
}

bool refreshDown() { return digitalRead(BTN_REFRESH) == LOW; }
bool nextDown() { return digitalRead(BTN_NEXT) == LOW; }

// Lives on USB; the battery pin's divider is not worth a gauge here.
int batteryPercent() { return -1; }
bool charging() { return false; }
}  // namespace board

#elif defined(AIRDASH_BOARD_STICKS3)

namespace {
uint8_t onBrightness = 0;   // what M5Unified chose at boot; restored on "on"
}

namespace board {
Gfx& display() { return M5.Display; }

void begin() {
  auto cfg = M5.config();
  cfg.serial_baudrate = 0;   // main.cpp opens Serial itself
  // M5Unified's S3 default fallback is the AtomS3 Lite; if autodetect ever
  // misses, that profile would leave the PMIC-gated panel dark.
  cfg.fallback_board = m5::board_t::board_M5StickS3;
  M5.begin(cfg);             // powers the LCD rail through the PMIC
  M5.Display.setRotation(1);
  Serial.printf("board %d display %d\n", (int)M5.getBoard(), (int)M5.Display.getBoard());
  onBrightness = M5.Display.getBrightness();
}

// M5Unified samples the buttons in update(); without it every loop the
// button levels below never change.
void update() { M5.update(); }

void setBacklight(bool on) { M5.Display.setBrightness(on ? onBrightness : 0); }

// M5GFX reads a plain uint16_t* as native (little-endian) RGB565 and
// swaps it for the panel; swap565_t marks the data as already in wire order.
void pushRgb565BE(int x, int y, int w, int h, const uint8_t* data) {
  M5.Display.pushImage(x, y, w, h, reinterpret_cast<const lgfx::swap565_t*>(data));
}

bool refreshDown() { return M5.BtnB.isPressed(); }   // side key
bool nextDown() { return M5.BtnA.isPressed(); }      // front key: next, hold = backlight

// The M5PM1 estimates the level from battery voltage; -1 on a failed read.
int batteryPercent() { return (int)M5.Power.getBatteryLevel(); }
bool charging() { return M5.Power.isCharging() == m5::Power_Class::is_charging; }
}  // namespace board

#endif
