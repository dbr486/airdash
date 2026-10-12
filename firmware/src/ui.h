#pragma once
#include <cstddef>
#include <cstdint>
#include "model.h"

namespace ui {
constexpr size_t LOGO_BYTES = 104 * 32 * 2;
void begin();
void showBoot(const char* ssid);
void showBootStatus(const char* line);
void showAircraft(const Nearest& n);
void showNoTraffic(const Nearest& n);
void toggleAliveDot();
void setMarks(bool stale, bool err);
void toggleBacklight();
void setBattery(int percent, bool charging);   // percent < 0 hides the icon
void showLogo(const uint8_t* rgb565);   // 104x32 RGB565 big-endian, LOGO_BYTES long
void clearLogo();
}
