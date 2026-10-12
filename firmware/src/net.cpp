#include "net.h"
#include <Arduino.h>
#include <WiFi.h>
#include <HTTPClient.h>
#include "parse.h"

namespace net {

void begin(const char* ssid, const char* pass) {
  WiFi.mode(WIFI_STA);
  // Modem sleep wakes the radio only on DTIM beacons; on the StickS3's small
  // antenna that stretched round trips past 1 s and half the 2 s polls failed.
  // Both displays live on USB power, so keep the radio awake.
  WiFi.setSleep(false);
  WiFi.setAutoReconnect(true);
  WiFi.persistent(false);
  WiFi.begin(ssid, pass);
}

bool waitForWifi(unsigned long timeoutMs) {
  const unsigned long start = millis();
  while (WiFi.status() != WL_CONNECTED) {
    if (millis() - start > timeoutMs) return false;
    delay(100);
  }
  return true;
}

bool connected() { return WiFi.status() == WL_CONNECTED; }

void retry() {
  Serial.println("wifi retry");
  WiFi.reconnect();
}

const char* localIp(char* buf, size_t len) {
  WiFi.localIP().toString().toCharArray(buf, len);
  return buf;
}

// One GET + parse. The body lands in HTTPClient's String (a few hundred
// bytes every 3 s); everything after that is fixed buffers.
PollResult poll(const char* url, Nearest& out) {
  if (!connected()) return PollResult::WifiDown;
  WiFiClient client;
  HTTPClient http;
  http.setConnectTimeout(2000);
  http.setTimeout(2000);
  if (!http.begin(client, url)) return PollResult::HttpError;
  const int code = http.GET();
  if (code != HTTP_CODE_OK) {
    http.end();
    Serial.printf("poll http=%d\n", code);
    return PollResult::HttpError;
  }
  const String body = http.getString();
  http.end();
  if (!parseNearest(body.c_str(), out)) {
    Serial.println("poll parse-error");
    return PollResult::ParseError;
  }
  return PollResult::Ok;
}

// Fetches one ready-made bitmap. The service sends exactly len bytes; a
// different Content-Length or a short read is treated as an error so a
// half-drawn logo never reaches the panel.
FetchResult fetchLogo(const char* url, uint8_t* buf, size_t len) {
  if (!connected()) return FetchResult::Error;
  WiFiClient client;
  HTTPClient http;
  http.setConnectTimeout(2000);
  http.setTimeout(2000);
  if (!http.begin(client, url)) return FetchResult::Error;
  const int code = http.GET();
  if (code == HTTP_CODE_NOT_FOUND) { http.end(); return FetchResult::NotFound; }
  if (code != HTTP_CODE_OK || http.getSize() != (int)len) {
    Serial.printf("logo http=%d size=%d\n", code, http.getSize());
    http.end();
    return FetchResult::Error;
  }
  const size_t got = http.getStream().readBytes(buf, len);
  http.end();
  return got == len ? FetchResult::Ok : FetchResult::Error;
}

}  // namespace net
