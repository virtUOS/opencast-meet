package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		realIP bool
		xff    string
		want   string
	}{
		{"disabled ignores header", false, "203.0.113.5", "10.0.0.1"},
		{"first entry wins", true, "203.0.113.5, 10.0.0.2", "203.0.113.5"},
		{"ipv6 with whitespace", true, " 2001:db8::1 ", "2001:db8::1"},
		{"garbage falls back", true, "garbage, 10.0.0.2", "10.0.0.1"},
		{"missing header falls back", true, "", "10.0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &server{config: Config{EnableRealIP: c.realIP}}
			r := httptest.NewRequest(http.MethodPost, "/join", nil)
			r.RemoteAddr = "10.0.0.1:1234"
			r.Header.Set("X-Real-IP", "198.51.100.9") // must never be used
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := s.clientIP(r); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestRateLimiter(t *testing.T) {
	rl := &rateLimiter{addrs: make(map[string]*addrState)}
	if d := rl.retryAfter("a"); d != 0 {
		t.Fatalf("unknown addr should not be blocked, got %v", d)
	}

	rl.recordFailure("a")
	if d := rl.retryAfter("a"); d <= 0 || d > rlDelayBase {
		t.Errorf("after one failure want block up to %v, got %v", rlDelayBase, d)
	}

	for i := 0; i < 100; i++ {
		rl.recordFailure("a")
	}
	if d := rl.retryAfter("a"); d > rlDelayMax {
		t.Errorf("block should be capped at %v, got %v", rlDelayMax, d)
	}
	if d := rl.retryAfter("b"); d != 0 {
		t.Errorf("other addr must not be affected, got %v", d)
	}
	if n := rl.activeIPs(); n != 1 {
		t.Errorf("want 1 tracked IP, got %d", n)
	}

	rl.reset("a")
	if d := rl.retryAfter("a"); d != 0 {
		t.Errorf("reset should clear block, got %v", d)
	}

	rl.recordFailure("stale")
	rl.addrs["stale"].lastSeen = time.Now().Add(-rlResetAfter - time.Minute)
	rl.recordFailure("fresh")
	if _, ok := rl.addrs["stale"]; ok {
		t.Error("stale entry should have been evicted")
	}
}

func TestBBBURL(t *testing.T) {
	base, _ := url.Parse("https://bbb.example.com/bigbluebutton/")
	cfg := Config{BBBURL: base, BBBSecret: "s3cret"}
	params := url.Values{"meetingID": {"room a"}, "fullName": {"Jäne & Co"}}

	got, err := url.Parse(bbbURL(cfg, "join", params))
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/bigbluebutton/api/join" {
		t.Errorf("path = %q", got.Path)
	}
	q := got.Query()
	if q.Get("fullName") != "Jäne & Co" || q.Get("meetingID") != "room a" {
		t.Errorf("params not preserved: %v", q)
	}

	// Checksum is SHA256(call + query-without-checksum + secret).
	q.Del("checksum")
	sum := sha256.Sum256([]byte("join" + q.Encode() + "s3cret"))
	if want := hex.EncodeToString(sum[:]); got.Query().Get("checksum") != want {
		t.Errorf("checksum = %q, want %q", got.Query().Get("checksum"), want)
	}
}

func TestCreateMeetingAndJoinURL(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`<response><returncode>SUCCESS</returncode>` +
			`<meetingID>r1</meetingID><createTime>123</createTime></response>`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/bbb/")
	cfg := Config{BBBURL: base, BBBSecret: "x", FrontendURL: "https://meet.example.com"}

	resp, err := createMeeting(cfg, Room{ID: "r1", Name: "Room", Record: true}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if resp.MeetingID != "r1" || resp.CreateTime != "123" {
		t.Errorf("unexpected response %+v", resp)
	}
	if gotQuery.Get("record") != "true" || gotQuery.Get("logoutURL") != cfg.FrontendURL {
		t.Errorf("unexpected create params %v", gotQuery)
	}

	join, _ := url.Parse(generateJoinURL(cfg, resp.MeetingID, resp.CreateTime, "Jane", "VIEWER"))
	q := join.Query()
	if q.Get("createTime") != "123" || q.Get("role") != "VIEWER" || q.Get("checksum") == "" {
		t.Errorf("unexpected join params %v", q)
	}
}

func TestIndexTemplateConsent(t *testing.T) {
	tmpl := template.Must(template.ParseFS(staticFiles, "static/index.html"))
	rooms := []Room{{ID: "a", Name: "A"}, {ID: "b", Name: "B", Record: true}}
	s := &server{config: Config{Rooms: rooms}}

	render := func(errCode, room string) string {
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, s.indexData(errCode, "", room)); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	out := render("password", "b")
	for _, want := range []string{
		errorMessages["password"],
		`data-record="false"`,
		`data-record="true"`,
		`B (rec)`,
		`id="consent-wrap" class="consent-wrap">`,
		`name="recording_consent" id="recording_consent" required>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}

	out = render("<b>bogus</b>", "a")
	if strings.Contains(out, "bogus") || !strings.Contains(out, `class="consent-wrap" hidden>`) {
		t.Errorf("unknown error code rendered or consent not hidden")
	}
}

func TestLoadRooms(t *testing.T) {
	set := func(kv ...string) {
		for i := 0; i < len(kv); i += 2 {
			t.Setenv(kv[i], kv[i+1])
		}
	}
	set("ROOM_1_ID", "a", "ROOM_1_NAME", "A", "ROOM_1_RECORD", "True",
		"ROOM_2_ID", "b", "ROOM_2_NAME", "B")
	rooms, err := loadRooms()
	if err != nil || len(rooms) != 2 {
		t.Fatalf("got %v rooms, err %v", len(rooms), err)
	}
	if !rooms[0].Record || rooms[1].Record {
		t.Errorf("record parsing wrong: %+v", rooms)
	}
	if findRoom(rooms, "missing").ID != "a" {
		t.Error("unknown room should fall back to the first room")
	}

	set("ROOM_2_NAME", "")
	if _, err := loadRooms(); err == nil || !strings.Contains(err.Error(), "ROOM_2_NAME") {
		t.Errorf("missing name should fail, got %v", err)
	}

	set("ROOM_2_NAME", "B", "ROOM_2_ID", "a")
	if _, err := loadRooms(); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Errorf("duplicate id should fail, got %v", err)
	}

	set("ROOM_1_ID", "")
	if _, err := loadRooms(); err == nil {
		t.Error("no rooms should fail")
	}
}
