#pragma once
#include <cstddef>
#include <cstdint>
#include "model.h"

namespace net {
void begin(const char* ssid, const char* pass);
bool waitForWifi(unsigned long timeoutMs);
bool connected();
void retry();   // explicit reconnect attempt while the link is down
const char* localIp(char* buf, size_t len);
PollResult poll(const char* url, Nearest& out);
FetchResult fetchLogo(const char* url, uint8_t* buf, size_t len);
}
