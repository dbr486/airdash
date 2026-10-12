#pragma once
#include <cstdint>
#if defined(AIRDASH_BOARD_STICKS3)
#include <M5Unified.h>
using Gfx = M5GFX;
#else
#include <TFT_eSPI.h>
using Gfx = TFT_eSPI;
#endif

// Everything that differs between the supported boards. ui.cpp and
// main.cpp never name a pin, a display driver or a power chip.
namespace board {
Gfx& display();
void begin();                 // power, panel init, landscape rotation
void update();                // once per loop, before reading buttons
void setBacklight(bool on);
// w*h pixels of big-endian RGB565 (the service's wire format).
void pushRgb565BE(int x, int y, int w, int h, const uint8_t* data);
bool refreshDown();           // true while the refresh button is held
bool nextDown();              // true while the next/backlight button is held
int batteryPercent();         // 0-100, or -1 when the board has no gauge
bool charging();
}
