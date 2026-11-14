package sourcelog

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"airdash/internal/capture"
)

func resp(source, subject string, at time.Time, body string) capture.Response {
	return capture.Response{
		Source: source, Subject: subject, URL: "https://example.test/" + subject,
		ContentType: "application/json", Status: 200, FetchedAt: at, Body: []byte(body),
	}
}

func mustWriteResponse(t *testing.T, l *Log, r capture.Response) {
	t.Helper()
	if err := l.writeResponse(r); err != nil {
		t.Fatalf("writeResponse: %v", err)
	}
}

func countResponses(t *testing.T, l *Log) int {
	t.Helper()
	var n int
	if err := l.db.QueryRow(`SELECT count(*) FROM responses`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestCaptureStoresCompressed(t *testing.T) {
	l, _ := openTest(t)
	body := strings.Repeat(`{"route":"AGP-STN"},`, 5000) // 100000 bytes
	mustWriteResponse(t, l, resp("adsbdb", "RYR5EL", t0, body))

	var size, stored int
	var blob []byte
	if err := l.db.QueryRow(`SELECT size, length(body), body FROM responses`).Scan(&size, &stored, &blob); err != nil {
		t.Fatalf("select: %v", err)
	}
	if size != 100000 {
		t.Errorf("size = %d, want 100000", size)
	}
	if stored >= 20000 {
		t.Errorf("stored %d bytes, want < 20000", stored)
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if string(got) != body {
		t.Error("gunzipped body differs from original")
	}
}

func TestCaptureNeverBlocks(t *testing.T) {
	l, _ := openTest(t)
	done := make(chan struct{})
	go func() {
		for i := 0; i < queueSize+10; i++ {
			l.Capture(resp("adsbdb", "X", t0, "{}"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Capture blocked with no writer running")
	}
}

func TestRunWritesCaptures(t *testing.T) {
	l, _ := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	l.Capture(resp("adsbdb", "RYR5EL", t0, `{"a":1}`))
	deadline := time.Now().Add(2 * time.Second)
	for countResponses(t, l) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("capture never written")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestPruneResponsesPerSource(t *testing.T) {
	l, _ := openTest(t) // clock is t0
	day := 24 * time.Hour
	mustWriteResponse(t, l, resp("airlabs", "dep:AGP", t0.Add(-8*day), "{}"))
	mustWriteResponse(t, l, resp("adsbdb", "KEEP", t0.Add(-8*day), "{}"))
	mustWriteResponse(t, l, resp("adsbdb", "OLD", t0.Add(-91*day), "{}"))
	if err := l.prune(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var subject string
	if n := countResponses(t, l); n != 1 {
		t.Fatalf("%d rows left, want 1", n)
	}
	if err := l.db.QueryRow(`SELECT subject FROM responses`).Scan(&subject); err != nil || subject != "KEEP" {
		t.Errorf("left %q (%v), want the 8-day adsbdb row", subject, err)
	}
}

func TestResponsesBeforeInclusiveNewestFirst(t *testing.T) {
	l, _ := openTest(t)
	mustWriteResponse(t, l, resp("adsbdb", "RYR5EL", t0.Add(-time.Minute), "{}"))
	mustWriteResponse(t, l, resp("adsbdb", "RYR5EL", t0, "{}"))
	mustWriteResponse(t, l, resp("adsbdb", "RYR5EL", t0.Add(time.Minute), "{}"))
	mustWriteResponse(t, l, resp("adsbdb", "OTHER", t0, "{}"))

	got, err := l.Responses(context.Background(), "adsbdb", "RYR5EL", t0, 10)
	if err != nil {
		t.Fatalf("Responses: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	if got[0].FetchedAt != t0.Unix() || got[1].FetchedAt != t0.Add(-time.Minute).Unix() {
		t.Errorf("order = %d, %d; want t0 then t0-60s", got[0].FetchedAt, got[1].FetchedAt)
	}
}

func TestResponsesAnySubject(t *testing.T) {
	l, _ := openTest(t)
	mustWriteResponse(t, l, resp("airlabs", "dep:AGP", t0, "{}"))
	mustWriteResponse(t, l, resp("airlabs", "arr:AGP", t0, "{}"))
	mustWriteResponse(t, l, resp("adsbdb", "X", t0, "{}"))
	got, err := l.Responses(context.Background(), "airlabs", "", t0, 10)
	if err != nil {
		t.Fatalf("Responses: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d rows, want both airlabs subjects", len(got))
	}
}

func TestResponsesEmptyIsNotNil(t *testing.T) {
	l, _ := openTest(t)
	got, err := l.Responses(context.Background(), "adsbdb", "none", time.Time{}, 10)
	if err != nil {
		t.Fatalf("Responses: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("got %#v, want empty non-nil slice", got)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	l, _ := openTest(t)
	in := resp("hexdb", "airport:LEMG", t0, `{"airport":"Malaga"}`)
	in.Status = 404
	mustWriteResponse(t, l, in)
	ms, err := l.Responses(context.Background(), "hexdb", "airport:LEMG", time.Time{}, 10)
	if err != nil || len(ms) != 1 {
		t.Fatalf("Responses: %v, %d rows", err, len(ms))
	}
	meta, body, found, err := l.Response(context.Background(), ms[0].ID)
	if err != nil || !found {
		t.Fatalf("Response: found=%v err=%v", found, err)
	}
	want := ResponseMeta{ID: ms[0].ID, Source: "hexdb", Subject: "airport:LEMG", URL: in.URL,
		Status: 404, ContentType: "application/json", FetchedAt: t0.Unix(), Size: len(in.Body)}
	if meta != want {
		t.Errorf("meta = %+v, want %+v", meta, want)
	}
	if string(body) != string(in.Body) {
		t.Errorf("body = %q", body)
	}
	if _, _, found, err := l.Response(context.Background(), 9999); found || err != nil {
		t.Errorf("unknown id: found=%v err=%v", found, err)
	}
}
