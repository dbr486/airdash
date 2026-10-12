#include <unity.h>
#include <cstdio>
#include <cstring>
#include <ArduinoJson.h>
#include "model.h"
#include "parse.h"
#include "format.h"
#include "buttons.h"
#include "reconnect.h"
#include "stale.h"
#include "logourl.h"
#include "logosync.h"
#include "pollurl.h"
#include "battery.h"

void setUp() {}
void tearDown() {}

static const char* FOUND =
  "{\"found\":true,\"callsign\":\"BAW466\",\"type\":\"A319\",\"reg\":\"G-EUPG\","
  "\"airline\":\"British Airways\",\"origin\":\"LHR\",\"dest\":\"BCN\","
  "\"alt_ft\":3525,\"gs_kt\":267,\"track\":216,\"dist_km\":3.4,\"bearing\":112,"
  "\"age_s\":2,\"nearby\":3,\"radius_km\":10}";

static const char* EMPTY = "{\"found\":false,\"age_s\":45,\"nearby\":0,\"radius_km\":12.5}";

void test_parses_found_body() {
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(FOUND, n));
  TEST_ASSERT_TRUE(n.found);
  TEST_ASSERT_EQUAL_STRING("BAW466", n.callsign);
  TEST_ASSERT_EQUAL_STRING("A319", n.type);
  TEST_ASSERT_EQUAL_STRING("G-EUPG", n.reg);
  TEST_ASSERT_EQUAL_STRING("British Airways", n.airline);
  TEST_ASSERT_EQUAL_STRING("LHR", n.origin);
  TEST_ASSERT_EQUAL_STRING("BCN", n.dest);
  TEST_ASSERT_EQUAL_INT(3525, n.altFt);
  TEST_ASSERT_EQUAL_INT(267, n.gsKt);
  TEST_ASSERT_EQUAL_INT(216, n.track);
  TEST_ASSERT_FLOAT_WITHIN(0.01f, 3.4f, n.distKm);
  TEST_ASSERT_EQUAL_INT(112, n.bearing);
  TEST_ASSERT_EQUAL_INT(2, n.ageS);
  TEST_ASSERT_EQUAL_INT(3, n.nearby);
  TEST_ASSERT_FLOAT_WITHIN(0.01f, 10.0f, n.radiusKm);
}

void test_parses_empty_body() {
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(EMPTY, n));
  TEST_ASSERT_FALSE(n.found);
  TEST_ASSERT_EQUAL_INT(45, n.ageS);
  TEST_ASSERT_EQUAL_INT(0, n.nearby);
  TEST_ASSERT_FLOAT_WITHIN(0.01f, 12.5f, n.radiusKm);
  TEST_ASSERT_EQUAL_STRING("", n.callsign);
}

void test_truncates_long_strings() {
  const char* body =
    "{\"found\":true,\"callsign\":\"ABCDEFGHIJKLMNOP\",\"type\":\"LONGTYPE9\","
    "\"reg\":\"REGISTRATION-TOO-LONG\",\"airline\":\"An Airline With A Very Very Long Name Indeed\","
    "\"origin\":\"EGLLX\",\"dest\":\"LEBLX\",\"alt_ft\":1,\"gs_kt\":1,\"track\":1,"
    "\"dist_km\":1,\"bearing\":1,\"age_s\":1,\"nearby\":1,\"radius_km\":1}";
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(body, n));
  TEST_ASSERT_EQUAL_size_t(11, strlen(n.callsign));
  TEST_ASSERT_EQUAL_size_t(7, strlen(n.type));
  TEST_ASSERT_EQUAL_size_t(11, strlen(n.reg));
  TEST_ASSERT_EQUAL_size_t(31, strlen(n.airline));
  TEST_ASSERT_EQUAL_STRING("EGLL", n.origin);
  TEST_ASSERT_EQUAL_STRING("LEBL", n.dest);
}

void test_malformed_and_missing_found() {
  Nearest n;
  TEST_ASSERT_FALSE(parseNearest("<html>502</html>", n));
  TEST_ASSERT_FALSE(parseNearest("{\"found\":true,\"callsign\":\"BAW", n));
  TEST_ASSERT_FALSE(parseNearest("{\"age_s\":1}", n));
  TEST_ASSERT_FALSE(parseNearest("", n));
}

// A recorded worst-case /api/nearest body: max-length strings for every
// field, plus the delay object the parser does not yet read (rendering is
// plan 4b). A 768-byte document ran out of room on this exact body on the
// live T-Display; this test pins the capacity that fixed it.
static const char* WORST_CASE =
  "{\"found\":true,\"callsign\":\"ABCDEFGH\",\"hex\":\"~abcdef\",\"index\":12,\"type\":\"B38M\",\"reg\":\"EC-MNOP\","
  "\"airline\":\"An Airline With A Very Long Name Ltd\",\"airline_iata\":\"XX\",\"logo\":\"XX\",\"origin\":\"EGLL\",\"dest\":\"LEMG\","
  "\"origin_label\":\"Palma de Mallorca Balea\",\"dest_label\":\"Frankfurt am Main Hesse\","
  "\"alt_ft\":12345,\"gs_kt\":456,\"track\":359,\"dist_km\":123.4,\"bearing\":359,"
  "\"age_s\":12,\"nearby\":12,\"radius_km\":250,"
  "\"delay\":{\"kind\":\"cancelled\",\"min\":-123,\"basis\":\"departure\"}}";

void test_worst_case_body_fits() {
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(WORST_CASE, n));

  StaticJsonDocument<1536> doc;
  DeserializationError err = deserializeJson(doc, WORST_CASE);
  TEST_ASSERT_FALSE(err);
  size_t used = doc.memoryUsage();
  printf("worst-case body memoryUsage() = %zu bytes (capacity 1536)\n", used);
  TEST_ASSERT_TRUE(used < (size_t)(1536 * 0.8));
}

void test_route_fallbacks() {
  char out[40];
  Nearest n;
  parseNearest(FOUND, n);
  formatRoute(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("LHR -> BCN", out);

  n.originLabel[0] = 0; n.destLabel[0] = 0;
  formatRoute(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("British Airways", out);

  n.airline[0] = 0;
  formatRoute(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("G-EUPG", out);

  n.reg[0] = 0;
  formatRoute(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("", out);

  // Only one end known: still no dangling arrow.
  strcpy(n.originLabel, "LHR");
  formatRoute(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("LHR -> ?", out);
}

void test_row_formats() {
  char out[48];
  Nearest n;
  parseNearest(FOUND, n);
  formatTypeAirline(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("A319  British Airways", out);
  formatAltSpeed(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("3525 ft  267 kt", out);
  formatDist(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("3.4 km  1/3", out);

  n.type[0] = 0;
  formatTypeAirline(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("British Airways", out);
  n.airline[0] = 0;
  formatTypeAirline(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("", out);
}

void test_radius_format() {
  char out[32];
  formatRadius(10.0f, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("within 10 km", out);
  formatRadius(12.5f, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("within 12.5 km", out);
  formatRadius(0.1f, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("within 0.1 km", out);
}

void test_button_fires_once_per_press_even_when_held() {
  Button b;
  TEST_ASSERT_TRUE(b.pressed(true, 1000));     // press edge
  TEST_ASSERT_FALSE(b.pressed(true, 1300));    // still held past the lockout
  TEST_ASSERT_FALSE(b.pressed(true, 2500));    // held 1.5 s: no repeat
  TEST_ASSERT_FALSE(b.pressed(false, 2600));   // release
  TEST_ASSERT_TRUE(b.pressed(true, 3000));     // second distinct press
}

void test_button_ignores_bounce_within_lockout() {
  Button b;
  TEST_ASSERT_TRUE(b.pressed(true, 1000));
  TEST_ASSERT_FALSE(b.pressed(false, 1050));   // bounce high inside the lockout
  TEST_ASSERT_FALSE(b.pressed(true, 1100));    // bounce low inside the lockout
  TEST_ASSERT_FALSE(b.pressed(true, 1300));    // still the same physical press
  TEST_ASSERT_FALSE(b.pressed(true, 1900));    // held for almost a second
  TEST_ASSERT_FALSE(b.pressed(false, 2000));   // real release
  TEST_ASSERT_TRUE(b.pressed(true, 2300));     // next press
}

void test_stale_ages_displayed_data_during_outage() {
  Staleness st;
  TEST_ASSERT_FALSE(st.update(true, false, 2, 1000));          // fresh render
  TEST_ASSERT_FALSE(st.update(false, false, 0, 11000));        // outage, 10 s old
  TEST_ASSERT_TRUE(st.update(false, false, 0, 32000));         // outage, 31 s old
  TEST_ASSERT_FALSE(st.update(true, false, 3, 33000));         // back: fresh again
  TEST_ASSERT_TRUE(st.update(true, false, 45, 36000));         // service says old
  TEST_ASSERT_TRUE(st.update(false, true, 0, 37000));          // parse error
  TEST_ASSERT_TRUE(st.update(false, false, 0, 38000));         // stays stale after a parse error
  TEST_ASSERT_FALSE(st.update(true, false, 1, 39000));         // cleared by a good poll
}

void test_stale_before_first_render_is_false() {
  Staleness st;
  TEST_ASSERT_FALSE(st.update(false, false, 0, 100000));       // nothing rendered yet: boot screen, not stale
}

void test_reconnect_due_every_10s_while_down() {
  Reconnector r;
  TEST_ASSERT_FALSE(r.due(true, 0));
  TEST_ASSERT_FALSE(r.due(true, 5000));
  TEST_ASSERT_FALSE(r.due(false, 6000));      // just dropped: auto-reconnect gets its chance
  TEST_ASSERT_FALSE(r.due(false, 11000));
  TEST_ASSERT_TRUE(r.due(false, 16000));      // 10 s after the drop
  TEST_ASSERT_FALSE(r.due(false, 20000));
  TEST_ASSERT_TRUE(r.due(false, 26000));      // 10 s after the last attempt
  TEST_ASSERT_FALSE(r.due(true, 27000));      // back up
  TEST_ASSERT_FALSE(r.due(false, 28000));     // new drop starts a fresh grace period
  TEST_ASSERT_TRUE(r.due(false, 38000));
}

static const char* FOUND_LATE =
  "{\"found\":true,\"callsign\":\"TRA39Z\",\"type\":\"B738\",\"reg\":\"PH-HXA\","
  "\"airline\":\"Transavia\",\"origin\":\"BRU\",\"dest\":\"AGP\",\"alt_ft\":3000,\"gs_kt\":200,"
  "\"track\":180,\"dist_km\":4.2,\"bearing\":90,\"age_s\":2,\"nearby\":2,\"radius_km\":10,"
  "\"delay\":{\"kind\":\"late\",\"min\":14,\"basis\":\"arrival\"}}";

void test_parse_delay_present() {
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(FOUND_LATE, n));
  TEST_ASSERT_TRUE(n.hasDelay);
  TEST_ASSERT_EQUAL_STRING("late", n.delayKind);
  TEST_ASSERT_EQUAL_INT(14, n.delayMin);
}

void test_parse_delay_absent() {
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(FOUND, n));   // the existing fixture has no delay object
  TEST_ASSERT_FALSE(n.hasDelay);
  TEST_ASSERT_EQUAL_STRING("", n.delayKind);
}

void test_parse_delay_unknown_kind_is_ignored() {
  const char* body =
    "{\"found\":true,\"callsign\":\"X\",\"delay\":{\"kind\":\"teleported\",\"min\":3},"
    "\"age_s\":1,\"nearby\":1,\"radius_km\":10}";
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(body, n));
  TEST_ASSERT_FALSE(n.hasDelay);
  char out[12];
  formatDelay(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("", out);
}

void test_format_delay_texts() {
  Nearest n;
  char out[12];
  n.hasDelay = true;
  strcpy(n.delayKind, "on_time"); n.delayMin = 2;  formatDelay(n, out, sizeof out); TEST_ASSERT_EQUAL_STRING("On time", out);
  strcpy(n.delayKind, "late");    n.delayMin = 14; formatDelay(n, out, sizeof out); TEST_ASSERT_EQUAL_STRING("Late 14m", out);
  strcpy(n.delayKind, "early");   n.delayMin = -7; formatDelay(n, out, sizeof out); TEST_ASSERT_EQUAL_STRING("Early 7m", out);
  strcpy(n.delayKind, "early");   n.delayMin = -120; formatDelay(n, out, sizeof out); TEST_ASSERT_EQUAL_STRING("Early 120m", out);
  strcpy(n.delayKind, "late");    n.delayMin = 999; formatDelay(n, out, sizeof out); TEST_ASSERT_EQUAL_STRING("Late 999m", out);
  strcpy(n.delayKind, "cancelled"); formatDelay(n, out, sizeof out); TEST_ASSERT_EQUAL_STRING("Cancelled", out);
  strcpy(n.delayKind, "diverted");  formatDelay(n, out, sizeof out); TEST_ASSERT_EQUAL_STRING("Diverted", out);
  n.hasDelay = false;               formatDelay(n, out, sizeof out); TEST_ASSERT_EQUAL_STRING("", out);
}

void test_parse_delay_min_is_clamped() {
  Nearest n;
  char out[12];

  // Test: large positive value clamped to 9999
  const char* large_positive =
    "{\"found\":true,\"callsign\":\"X\",\"delay\":{\"kind\":\"late\",\"min\":123456},"
    "\"age_s\":1,\"nearby\":1,\"radius_km\":10}";
  TEST_ASSERT_TRUE(parseNearest(large_positive, n));
  TEST_ASSERT_EQUAL_INT(9999, n.delayMin);

  // Test: large negative value clamped to -9999
  const char* large_negative =
    "{\"found\":true,\"callsign\":\"X\",\"delay\":{\"kind\":\"late\",\"min\":-2147483648},"
    "\"age_s\":1,\"nearby\":1,\"radius_km\":10}";
  TEST_ASSERT_TRUE(parseNearest(large_negative, n));
  TEST_ASSERT_EQUAL_INT(-9999, n.delayMin);

  // Test: formatDelay with clamped max value fits in 12-byte buffer
  n.hasDelay = true;
  strcpy(n.delayKind, "late");
  n.delayMin = 9999;
  formatDelay(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("Late 9999m", out);
}

void test_parse_labels_present() {
  const char* body =
    "{\"found\":true,\"callsign\":\"BAW466\",\"origin\":\"LHR\",\"dest\":\"BCN\","
    "\"origin_label\":\"London\",\"dest_label\":\"Barcelona\",\"age_s\":1,\"nearby\":1,\"radius_km\":10}";
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(body, n));
  TEST_ASSERT_EQUAL_STRING("LHR", n.origin);
  TEST_ASSERT_EQUAL_STRING("London", n.originLabel);
  TEST_ASSERT_EQUAL_STRING("Barcelona", n.destLabel);
  char out[48];
  formatRoute(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("London -> Barcelona", out);
}

void test_parse_labels_fall_back_to_codes() {
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(FOUND, n));   // FOUND has no label keys
  TEST_ASSERT_EQUAL_STRING("LHR", n.originLabel);
  TEST_ASSERT_EQUAL_STRING("BCN", n.destLabel);
  char out[48];
  formatRoute(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("LHR -> BCN", out);
}

void test_parse_labels_truncate() {
  const char* body =
    "{\"found\":true,\"callsign\":\"X\",\"origin\":\"PMI\",\"dest\":\"FRA\","
    "\"origin_label\":\"Palma de Mallorca Balearics\",\"dest_label\":\"Frankfurt am Main Hessen\","
    "\"age_s\":1,\"nearby\":1,\"radius_km\":10}";
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(body, n));
  TEST_ASSERT_EQUAL_INT(23, strlen(n.originLabel));
  TEST_ASSERT_EQUAL_STRING("Palma de Mallorca Balea", n.originLabel);
}

void test_parse_logo_field() {
  const char* body = "{\"found\":true,\"callsign\":\"X\",\"logo\":\"FR\",\"age_s\":1,\"nearby\":1,\"radius_km\":10}";
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(body, n));
  TEST_ASSERT_EQUAL_STRING("FR", n.logo);
  Nearest m;
  TEST_ASSERT_TRUE(parseNearest(FOUND, m));   // no logo key -> ""
  TEST_ASSERT_EQUAL_STRING("", m.logo);
  const char* longBody = "{\"found\":true,\"callsign\":\"X\",\"logo\":\"FRX\",\"age_s\":1,\"nearby\":1,\"radius_km\":10}";
  TEST_ASSERT_TRUE(parseNearest(longBody, n));
  TEST_ASSERT_EQUAL_STRING("FR", n.logo);
}

void test_logo_url_from_nearest_url() {
  char out[96];
  TEST_ASSERT_TRUE(logoUrl("http://192.0.2.10:8080/api/nearest", "FR", out, sizeof out));
  TEST_ASSERT_EQUAL_STRING("http://192.0.2.10:8080/api/logo/FR/104x32.rgb565", out);
}

void test_logo_url_rejects_foreign_suffix() {
  char out[96];
  TEST_ASSERT_FALSE(logoUrl("http://host/other", "FR", out, sizeof out));
  TEST_ASSERT_FALSE(logoUrl("http://host/api/nearest", "", out, sizeof out));
  char tiny[20];
  TEST_ASSERT_FALSE(logoUrl("http://192.0.2.10:8080/api/nearest", "FR", tiny, sizeof tiny));
}

void test_logosync_fetches_only_on_change() {
  LogoSync ls;
  TEST_ASSERT_EQUAL(LogoSync::Action::Fetch, ls.decide("FR", false, 1000));
  TEST_ASSERT_EQUAL(LogoSync::After::Drawn, ls.result(FetchResult::Ok, "FR", 1000));
  TEST_ASSERT_EQUAL(LogoSync::Action::None, ls.decide("FR", false, 4000));
  TEST_ASSERT_EQUAL(LogoSync::Action::Fetch, ls.decide("BA", false, 7000));
}

void test_logosync_empty_code_clears_once() {
  LogoSync ls;
  ls.decide("FR", false, 0);
  ls.result(FetchResult::Ok, "FR", 0);
  TEST_ASSERT_EQUAL(LogoSync::Action::Clear, ls.decide("", false, 3000));
  TEST_ASSERT_EQUAL(LogoSync::Action::None, ls.decide("", false, 6000));
  // toggled back on: fetched again without waiting for a new aircraft
  TEST_ASSERT_EQUAL(LogoSync::Action::Fetch, ls.decide("FR", false, 9000));
}

void test_logosync_404_suppressed_then_retried_after_an_hour() {
  LogoSync ls;
  ls.decide("FR", false, 1000);
  TEST_ASSERT_EQUAL(LogoSync::After::Cleared, ls.result(FetchResult::NotFound, "FR", 1000));
  TEST_ASSERT_EQUAL(LogoSync::Action::None, ls.decide("FR", false, 4000));
  TEST_ASSERT_EQUAL(LogoSync::Action::None, ls.decide("FR", false, 1000 + 3599000UL));
  TEST_ASSERT_EQUAL(LogoSync::Action::Fetch, ls.decide("FR", false, 1000 + 3600000UL));
  // and the setting going off resets the memory outright
  ls.result(FetchResult::NotFound, "FR", 5000000UL);
  ls.decide("", false, 5003000UL);
  TEST_ASSERT_EQUAL(LogoSync::Action::Fetch, ls.decide("FR", false, 5006000UL));
}

void test_logosync_error_on_changed_code_clears_and_retries() {
  LogoSync ls;
  ls.decide("FR", false, 0);
  ls.result(FetchResult::Ok, "FR", 0);
  TEST_ASSERT_EQUAL(LogoSync::Action::Fetch, ls.decide("BA", false, 3000));
  // the old airline's pixels must not stay beside the new callsign
  TEST_ASSERT_EQUAL(LogoSync::After::Cleared, ls.result(FetchResult::Error, "BA", 3000));
  TEST_ASSERT_EQUAL(LogoSync::Action::Fetch, ls.decide("BA", false, 6000));
  // an error with nothing shown is just a retry
  TEST_ASSERT_EQUAL(LogoSync::After::Retry, ls.result(FetchResult::Error, "BA", 6000));
}

void test_logosync_rebuild_refetches_same_code() {
  LogoSync ls;
  ls.decide("FR", false, 0);
  ls.result(FetchResult::Ok, "FR", 0);
  ls.reset();   // screen was rebuilt (no-traffic in between)
  TEST_ASSERT_EQUAL(LogoSync::Action::Fetch, ls.decide("FR", true, 3000));
}

void test_parse_hex_and_index() {
  const char* body = "{\"found\":true,\"callsign\":\"X\",\"hex\":\"~3c6444\",\"index\":2,\"age_s\":1,\"nearby\":3,\"radius_km\":10}";
  Nearest n;
  TEST_ASSERT_TRUE(parseNearest(body, n));
  TEST_ASSERT_EQUAL_STRING("~3c6444", n.hex);
  TEST_ASSERT_EQUAL_INT(2, n.index);
  Nearest m;
  TEST_ASSERT_TRUE(parseNearest(FOUND, m));
  TEST_ASSERT_EQUAL_INT(0, m.index);
}

void test_dist_marker_only_when_several() {
  char out[40];
  Nearest n;
  parseNearest(FOUND, n);
  n.index = 1; n.nearby = 3;
  formatDist(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("3.4 km  2/3", out);
  n.nearby = 1; n.index = 0;
  formatDist(n, out, sizeof out);
  TEST_ASSERT_EQUAL_STRING("3.4 km", out);
}

void test_poll_url_variants() {
  char out[128];
  TEST_ASSERT_TRUE(pollUrl("http://h:8080/api/nearest", "", "", out, sizeof out));
  TEST_ASSERT_EQUAL_STRING("http://h:8080/api/nearest", out);
  TEST_ASSERT_TRUE(pollUrl("http://h:8080/api/nearest", "hex", "~3c6444", out, sizeof out));
  TEST_ASSERT_EQUAL_STRING("http://h:8080/api/nearest?hex=~3c6444", out);
  TEST_ASSERT_TRUE(pollUrl("http://h:8080/api/nearest", "after", "", out, sizeof out));
  TEST_ASSERT_EQUAL_STRING("http://h:8080/api/nearest?after=", out);
  char tiny[10];
  TEST_ASSERT_FALSE(pollUrl("http://h:8080/api/nearest", "hex", "abc", tiny, sizeof tiny));
}

void test_button_short_press_fires_on_release() {
  Button b;
  TEST_ASSERT_EQUAL(Press::None, b.hold(true, 1000));    // press edge
  TEST_ASSERT_EQUAL(Press::None, b.hold(true, 1300));    // held 300 ms
  TEST_ASSERT_EQUAL(Press::Short, b.hold(false, 1400));  // released before 700 ms
  TEST_ASSERT_EQUAL(Press::None, b.hold(false, 1500));
}

void test_button_long_press_does_not_fire_short() {
  Button b;
  TEST_ASSERT_EQUAL(Press::None, b.hold(true, 1000));
  TEST_ASSERT_EQUAL(Press::None, b.hold(true, 1600));
  TEST_ASSERT_EQUAL(Press::Long, b.hold(true, 1700));    // crosses 700 ms while held
  TEST_ASSERT_EQUAL(Press::None, b.hold(true, 2500));    // still held: fires once
  TEST_ASSERT_EQUAL(Press::None, b.hold(false, 2600));   // release: no short
  TEST_ASSERT_EQUAL(Press::None, b.hold(true, 2900));    // next press edge
  TEST_ASSERT_EQUAL(Press::Short, b.hold(false, 3200));
}

void test_button_hold_ignores_bounce() {
  Button b;
  b.hold(true, 1000);
  TEST_ASSERT_EQUAL(Press::None, b.hold(false, 1050));   // bounce inside the lockout
  TEST_ASSERT_EQUAL(Press::None, b.hold(true, 1100));
  TEST_ASSERT_EQUAL(Press::Short, b.hold(false, 1400));  // the real release
}

void test_button_long_hold_seen_only_at_release() {
  // The loop blocked (a poll or logo fetch) for the whole hold: the first
  // sample after the press edge is the release, well past the threshold.
  Button b;
  TEST_ASSERT_EQUAL(Press::None, b.hold(true, 1000));
  TEST_ASSERT_EQUAL(Press::Long, b.hold(false, 2500));
  TEST_ASSERT_EQUAL(Press::None, b.hold(false, 2600));
}

void test_battery_unknown_draws_nothing() {
  TEST_ASSERT_EQUAL(-1, batteryBars(-1));
  TEST_ASSERT_EQUAL(-1, batteryBars(-20));
}

void test_battery_bar_edges() {
  TEST_ASSERT_EQUAL(0, batteryBars(0));
  TEST_ASSERT_EQUAL(1, batteryBars(1));
  TEST_ASSERT_EQUAL(1, batteryBars(25));
  TEST_ASSERT_EQUAL(2, batteryBars(26));
  TEST_ASSERT_EQUAL(2, batteryBars(50));
  TEST_ASSERT_EQUAL(3, batteryBars(51));
  TEST_ASSERT_EQUAL(3, batteryBars(75));
  TEST_ASSERT_EQUAL(4, batteryBars(76));
  TEST_ASSERT_EQUAL(4, batteryBars(100));
  TEST_ASSERT_EQUAL(4, batteryBars(130));   // gauges overshoot on a fresh charge
}

void test_battery_colour_bands() {
  TEST_ASSERT_EQUAL((int)BatteryBand::Low, (int)batteryBand(0));
  TEST_ASSERT_EQUAL((int)BatteryBand::Low, (int)batteryBand(19));
  TEST_ASSERT_EQUAL((int)BatteryBand::Mid, (int)batteryBand(20));
  TEST_ASSERT_EQUAL((int)BatteryBand::Mid, (int)batteryBand(50));
  TEST_ASSERT_EQUAL((int)BatteryBand::High, (int)batteryBand(51));
  TEST_ASSERT_EQUAL((int)BatteryBand::High, (int)batteryBand(100));
}

int main(int, char**) {
  UNITY_BEGIN();
  RUN_TEST(test_parses_found_body);
  RUN_TEST(test_parses_empty_body);
  RUN_TEST(test_truncates_long_strings);
  RUN_TEST(test_malformed_and_missing_found);
  RUN_TEST(test_worst_case_body_fits);
  RUN_TEST(test_route_fallbacks);
  RUN_TEST(test_row_formats);
  RUN_TEST(test_radius_format);
  RUN_TEST(test_button_fires_once_per_press_even_when_held);
  RUN_TEST(test_button_ignores_bounce_within_lockout);
  RUN_TEST(test_reconnect_due_every_10s_while_down);
  RUN_TEST(test_stale_ages_displayed_data_during_outage);
  RUN_TEST(test_stale_before_first_render_is_false);
  RUN_TEST(test_parse_delay_present);
  RUN_TEST(test_parse_delay_absent);
  RUN_TEST(test_parse_delay_unknown_kind_is_ignored);
  RUN_TEST(test_format_delay_texts);
  RUN_TEST(test_parse_delay_min_is_clamped);
  RUN_TEST(test_parse_labels_present);
  RUN_TEST(test_parse_labels_fall_back_to_codes);
  RUN_TEST(test_parse_labels_truncate);
  RUN_TEST(test_parse_logo_field);
  RUN_TEST(test_logo_url_from_nearest_url);
  RUN_TEST(test_logo_url_rejects_foreign_suffix);
  RUN_TEST(test_logosync_fetches_only_on_change);
  RUN_TEST(test_logosync_empty_code_clears_once);
  RUN_TEST(test_logosync_404_suppressed_then_retried_after_an_hour);
  RUN_TEST(test_logosync_error_on_changed_code_clears_and_retries);
  RUN_TEST(test_logosync_rebuild_refetches_same_code);
  RUN_TEST(test_parse_hex_and_index);
  RUN_TEST(test_dist_marker_only_when_several);
  RUN_TEST(test_poll_url_variants);
  RUN_TEST(test_button_short_press_fires_on_release);
  RUN_TEST(test_button_long_press_does_not_fire_short);
  RUN_TEST(test_button_hold_ignores_bounce);
  RUN_TEST(test_button_long_hold_seen_only_at_release);
  RUN_TEST(test_battery_unknown_draws_nothing);
  RUN_TEST(test_battery_bar_edges);
  RUN_TEST(test_battery_colour_bands);
  return UNITY_END();
}
