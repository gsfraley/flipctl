package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterRefillsPerPlugin(t *testing.T) {
	l := newRateLimiter(60, 2)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("field.lora"); !ok {
			t.Fatalf("burst token %d refused", i)
		}
	}
	ok, wait := l.allow("field.lora")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("third start allowed=%v wait=%v", ok, wait)
	}
	if ok, _ := l.allow("network.ping"); !ok {
		t.Fatal("limit leaked across plugins")
	}
	now = now.Add(time.Second)
	if ok, _ := l.allow("field.lora"); !ok {
		t.Fatal("token not refilled after 1s at 60/min")
	}
	if newRateLimiter(0, 5) != nil {
		t.Fatal("zero rate should disable limiting")
	}
}

func TestJobStartIsRateLimitedByDaemon(t *testing.T) {
	apps, err := loadTestPluginApps("plugins")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(apps, nil)
	srv.limiter = newRateLimiter(1, 2)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	// Use the canonical helper for the allowed starts so they are real jobs.
	startCanonicalJob(t, ts, "network.ping", map[string]string{"target": "127.0.0.1", "count": "1"})
	startCanonicalJob(t, ts, "network.ping", map[string]string{"target": "127.0.0.1", "count": "1"})
	session := createSession(t, ts)
	home := getCanonicalDoc(t, ts, session.ID)
	opened := postSessionActionDoc(t, ts, session.ID, map[string]any{"actionId": "app.open", "viewId": home.ViewID, "revision": home.Revision, "params": map[string]string{"appId": "network.ping"}})
	b, _ := json.Marshal(map[string]any{"actionId": "job.start", "viewId": opened.ViewID, "revision": opened.Revision, "params": map[string]string{"appId": "network.ping"}})
	resp, err := http.Post(ts.URL+"/api/v1/sessions/"+session.ID+"/actions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("third start status %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}
