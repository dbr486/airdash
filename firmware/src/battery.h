#pragma once

// Rough battery reading for the corner icon: four bars, each a quarter of
// the charge (rounded up, so any charge left shows at least one bar), and
// a colour band. A negative percent means the board has no gauge or the
// read failed, and the icon is not drawn at all.
enum class BatteryBand { Low, Mid, High };

inline int batteryBars(int percent) {
  if (percent < 0) return -1;
  if (percent > 100) percent = 100;
  return (percent + 24) / 25;
}

inline BatteryBand batteryBand(int percent) {
  if (percent < 20) return BatteryBand::Low;
  if (percent <= 50) return BatteryBand::Mid;
  return BatteryBand::High;
}
