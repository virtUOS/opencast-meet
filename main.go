package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed static/index.html static/css static/img static/js
var staticFiles embed.FS

// ///
// VersionResponse represents the XML response from the /bigbluebutton/api/ endpoint
type VersionResponse struct {
	XMLName    xml.Name `xml:"response"`
	ReturnCode string   `xml:"returncode"`
	BBBVersion string   `xml:"bbbVersion"`
}

// CreateResponse represents the XML response from the create API endpoint
type CreateResponse struct {
	XMLName    xml.Name `xml:"response"`
	ReturnCode string   `xml:"returncode"`
	Message    string   `xml:"message"`
	MessageKey string   `xml:"messageKey"`
	MeetingID  string   `xml:"meetingID"`
	CreateTime string   `xml:"createTime"`
}

// Room is a single BBB meeting room entry.
type Room struct {
	ID                      string
	Name                    string
	Record                  bool
	WelcomeMessage          string
	PreUploadedPresentation string
	OCSeriesID              string
	OCDCCreator             string
	OCAddWebcams            string
	OCACLReadRoles          string
	OCACLWriteRoles         string
	AppendDate              bool
}

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	BBBURL    *url.URL
	BBBSecret string

	Rooms       []Room
	FrontendURL string

	UserPassword      string
	ModeratorPassword string
	ListenAddr        string

	MetricsUsername string
	MetricsPassword string

	EnableRealIP bool
}

type server struct {
	config     Config
	tmpl       *template.Template
	httpClient *http.Client
	limiter    *rateLimiter
}

var (
	metricJoinAttempts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_meet_join_attempts_total",
		Help: "Total join form submissions",
	})
	metricJoinInvalidPw = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_meet_join_invalid_password_total",
		Help: "Join attempts with invalid password",
	})
	metricJoinModerator = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_meet_join_moderator_total",
		Help: "Logins with moderator credential",
	})
	metricJoinViewer = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_meet_join_viewer_total",
		Help: "Logins with viewer credential",
	})
	metricBBBErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_meet_bbb_errors_total",
		Help: "Failures from BigBlueButton API calls",
	})
	metricJoinRateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_meet_join_rate_limited_total",
		Help: "Join attempts rejected because the client IP was rate limited",
	})
)

func init() {
	prometheus.MustRegister(
		metricJoinAttempts,
		metricJoinInvalidPw,
		metricJoinModerator,
		metricJoinViewer,
		metricBBBErrors,
		metricJoinRateLimited,
	)
}

type rateLimiter struct {
	mu    sync.Mutex
	addrs map[string]*addrState
}

type addrState struct {
	failures     int
	lastSeen     time.Time
	blockedUntil time.Time
}

// maxNameLength caps the participant name passed to BBB.
const maxNameLength = 100

const (
	rlResetAfter = 10 * time.Minute
	rlDelayBase  = 2 * time.Second
	rlDelayMax   = 30 * time.Second
)

// retryAfter reports how long addr must wait before another attempt is allowed.
// It returns 0 when addr is not currently blocked and does not mutate state.
func (rl *rateLimiter) retryAfter(addr string) time.Duration {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.evictStale()
	s := rl.addrs[addr]
	if s == nil {
		return 0
	}
	if d := time.Until(s.blockedUntil); d > 0 {
		return d
	}
	return 0
}

// recordFailure increments the failure counter for addr and extends its block
// window with exponential backoff (failures × rlDelayBase, capped at rlDelayMax).
func (rl *rateLimiter) recordFailure(addr string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.evictStale()
	s := rl.addrs[addr]
	if s == nil {
		s = &addrState{}
		rl.addrs[addr] = s
	}
	s.failures++
	now := time.Now()
	s.lastSeen = now
	d := time.Duration(s.failures) * rlDelayBase
	if d > rlDelayMax {
		d = rlDelayMax
	}
	s.blockedUntil = now.Add(d)
}

// reset clears any tracked state for addr, called after a successful login.
func (rl *rateLimiter) reset(addr string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.addrs, addr)
}

func (rl *rateLimiter) activeIPs() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.addrs)
}

// evictStale removes entries quiet for longer than rlResetAfter.
// Must be called with rl.mu held.
func (rl *rateLimiter) evictStale() {
	cutoff := time.Now().Add(-rlResetAfter)
	for k, s := range rl.addrs {
		if s.lastSeen.Before(cutoff) {
			delete(rl.addrs, k)
		}
	}
}

// clientIP returns the client's IP address. When EnableRealIP is set, the
// first entry of X-Forwarded-For is used. This relies on the outermost reverse
// proxy replacing any client-supplied X-Forwarded-For header (Caddy does so by
// default; nginx needs proxy_set_header X-Forwarded-For $remote_addr or the
// realip module). Invalid values fall back to the socket address.
func (s *server) clientIP(r *http.Request) string {
	if s.config.EnableRealIP {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			first := strings.TrimSpace(strings.SplitN(v, ",", 2)[0])
			if ip := net.ParseIP(first); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// bbbURL builds a signed BigBlueButton API URL for the given call. The
// checksum is SHA256(call + query + secret) as required by the BBB API.
func bbbURL(cfg Config, call string, params url.Values) string {
	sum := sha256.Sum256([]byte(call + params.Encode() + cfg.BBBSecret))
	params.Set("checksum", hex.EncodeToString(sum[:]))
	u := cfg.BBBURL.JoinPath("api", call)
	u.RawQuery = params.Encode()
	return u.String()
}

// bbbGet performs a GET request against apiURL and decodes the XML response
// into out.
func bbbGet(client *http.Client, apiURL string, out any) error {
	resp, err := client.Get(apiURL)
	if err != nil {
		return fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()
	if err := xml.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("failed to parse XML response: %w", err)
	}
	return nil
}

// getBBBVersion fetches the BigBlueButton server version.
func getBBBVersion(cfg Config, client *http.Client) (*VersionResponse, error) {
	var versionResp VersionResponse
	if err := bbbGet(client, cfg.BBBURL.JoinPath("api").String(), &versionResp); err != nil {
		return nil, err
	}
	return &versionResp, nil
}

// createMeeting ensures the configured meeting room exists on the BBB server.
// BBB is idempotent on meetingID: returns the existing meeting if already running,
// or creates a fresh one if it has ended.
func createMeeting(cfg Config, room Room, client *http.Client) (*CreateResponse, error) {
	params := url.Values{}
	params.Add("meetingID", room.ID)
	name := room.Name
	if room.AppendDate {
		name += " | " + time.Now().Format("2006-01-02")
	}
	params.Add("name", name)
	params.Add("muteOnStart", "true")
	params.Add("autoStartRecording", "false")
	params.Add("record", strconv.FormatBool(room.Record))
	params.Add("allowStartStopRecording", strconv.FormatBool(room.Record))
	if cfg.FrontendURL != "" {
		params.Add("loginURL", cfg.FrontendURL)
		params.Add("logoutURL", cfg.FrontendURL)
	}
	if room.WelcomeMessage != "" {
		params.Add("welcome", room.WelcomeMessage)
	}
	if room.PreUploadedPresentation != "" {
		params.Add("preUploadedPresentation", room.PreUploadedPresentation)
	}
	if room.OCSeriesID != "" {
		params.Add("meta_opencast-dc-isPartOf", room.OCSeriesID)
		params.Add("meta_opencast-dc-title", name)
	}
	if room.OCDCCreator != "" {
		params.Add("meta_opencast-dc-creator", room.OCDCCreator)
	}
	if room.OCAddWebcams != "" {
		params.Add("meta_opencast-add-webcams", room.OCAddWebcams)
	}
	if room.OCACLReadRoles != "" {
		params.Add("meta_opencast-acl-read-roles", room.OCACLReadRoles)
	}
	if room.OCACLWriteRoles != "" {
		params.Add("meta_opencast-acl-write-roles", room.OCACLWriteRoles)
	}

	var createResp CreateResponse
	if err := bbbGet(client, bbbURL(cfg, "create", params), &createResp); err != nil {
		return nil, err
	}
	return &createResp, nil
}

// generateJoinURL builds a BBB join URL for the given participant name and role.
// createTime binds the link to the current meeting instance so it stops working
// once that meeting has ended.
func generateJoinURL(cfg Config, meetingID, createTime, fullName, role string) string {
	params := url.Values{}
	params.Add("fullName", fullName)
	params.Add("meetingID", meetingID)
	params.Add("role", role)
	if createTime != "" {
		params.Add("createTime", createTime)
	}
	return bbbURL(cfg, "join", params)
}

// errorMessages maps the short error codes used in the ?error= query parameter
// to the text shown on the login page. Unknown codes render no message, so the
// page cannot be used to display arbitrary attacker-supplied text.
var errorMessages = map[string]string{
	"name":      "Please enter your name.",
	"password":  "Invalid password. Please try again.",
	"consent":   "You must consent to the recording to enter this room.",
	"ratelimit": "Too many attempts. Please wait a moment and try again.",
	"bbb":       "Could not connect to the meeting server. Please try again later.",
	"bbbfail":   "The meeting server returned an error. Please try again later.",
}

type indexTemplateData struct {
	Error                  string
	Name                   string
	Rooms                  []Room
	SelectedRoom           string
	SelectedRoomRecordable bool
}

func (s *server) indexData(errCode, name, selectedRoom string) indexTemplateData {
	room := findRoom(s.config.Rooms, selectedRoom)
	return indexTemplateData{
		Error:                  errorMessages[errCode],
		Name:                   name,
		Rooms:                  s.config.Rooms,
		SelectedRoom:           selectedRoom,
		SelectedRoomRecordable: room.Record,
	}
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.tmpl.Execute(w, s.indexData(
		r.URL.Query().Get("error"),
		r.URL.Query().Get("name"),
		r.URL.Query().Get("room"),
	))
}

func (s *server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	metricJoinAttempts.Inc()

	name := strings.TrimSpace(r.FormValue("name"))
	password := r.FormValue("password")
	roomID := r.FormValue("room")
	ip := s.clientIP(r)

	room := findRoom(s.config.Rooms, roomID)

	renderError := func(code string) {
		redirectURL := "/?error=" + code
		if name != "" {
			redirectURL += "&name=" + url.QueryEscape(name)
		}
		if len(s.config.Rooms) > 1 {
			redirectURL += "&room=" + url.QueryEscape(room.ID)
		}
		http.Redirect(w, r, redirectURL, http.StatusSeeOther)
	}

	if d := s.limiter.retryAfter(ip); d > 0 {
		metricJoinRateLimited.Inc()
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = s.tmpl.Execute(w, s.indexData("ratelimit", name, room.ID))
		return
	}

	if name == "" || len(name) > maxNameLength {
		renderError("name")
		return
	}

	var role string
	switch {
	case subtle.ConstantTimeCompare([]byte(password), []byte(s.config.ModeratorPassword)) == 1:
		role = "MODERATOR"
		metricJoinModerator.Inc()
	case subtle.ConstantTimeCompare([]byte(password), []byte(s.config.UserPassword)) == 1:
		role = "VIEWER"
		metricJoinViewer.Inc()
	default:
		s.limiter.recordFailure(ip)
		metricJoinInvalidPw.Inc()
		renderError("password")
		return
	}
	s.limiter.reset(ip)

	if room.Record && r.FormValue("recording_consent") != "on" {
		renderError("consent")
		return
	}

	createResp, err := createMeeting(s.config, room, s.httpClient)
	if err != nil {
		log.Printf("error creating meeting: %v", err)
		metricBBBErrors.Inc()
		renderError("bbb")
		return
	}
	if createResp.ReturnCode != "SUCCESS" {
		log.Printf("BBB create returned non-success: %s / %s", createResp.MessageKey, createResp.Message)
		metricBBBErrors.Inc()
		renderError("bbbfail")
		return
	}

	joinURL := generateJoinURL(s.config, createResp.MeetingID, createResp.CreateTime, name, role)
	http.Redirect(w, r, joinURL, http.StatusFound)
}

// securityHeaders sets conservative browser security headers on every
// response. The page only loads same-origin resources; the data: image source
// is for the inline SVG dropdown arrow in the stylesheet.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

func basicAuthHandler(username, password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="metrics"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loadRooms() ([]Room, error) {
	var rooms []Room
	seen := map[string]int{}
	for i := 1; ; i++ {
		id := os.Getenv(fmt.Sprintf("ROOM_%d_ID", i))
		if id == "" {
			break
		}
		if prev, dup := seen[id]; dup {
			return nil, fmt.Errorf("ROOM_%d_ID duplicates ROOM_%d_ID (%q)", i, prev, id)
		}
		seen[id] = i
		name := os.Getenv(fmt.Sprintf("ROOM_%d_NAME", i))
		if name == "" {
			return nil, fmt.Errorf("ROOM_%d_NAME is not set", i)
		}
		rooms = append(rooms, Room{
			ID:                      id,
			Name:                    name,
			Record:                  strings.EqualFold(os.Getenv(fmt.Sprintf("ROOM_%d_RECORD", i)), "true"),
			WelcomeMessage:          os.Getenv(fmt.Sprintf("ROOM_%d_WELCOME_MESSAGE", i)),
			PreUploadedPresentation: os.Getenv(fmt.Sprintf("ROOM_%d_PRE_UPLOADED_PRESENTATION", i)),
			OCSeriesID:              os.Getenv(fmt.Sprintf("ROOM_%d_OC_SERIES", i)),
			OCDCCreator:             os.Getenv(fmt.Sprintf("ROOM_%d_OC_DC_CREATOR", i)),
			OCAddWebcams:            os.Getenv(fmt.Sprintf("ROOM_%d_OC_ADD_WEBCAMS", i)),
			OCACLReadRoles:          os.Getenv(fmt.Sprintf("ROOM_%d_OC_ACL_READ_ROLES", i)),
			OCACLWriteRoles:         os.Getenv(fmt.Sprintf("ROOM_%d_OC_ACL_WRITE_ROLES", i)),
			AppendDate:              strings.EqualFold(os.Getenv(fmt.Sprintf("ROOM_%d_APPEND_DATE", i)), "true"),
		})
	}
	if len(rooms) == 0 {
		return nil, fmt.Errorf("no rooms configured: set at least ROOM_1_ID and ROOM_1_NAME")
	}
	return rooms, nil
}

func findRoom(rooms []Room, id string) Room {
	for _, r := range rooms {
		if r.ID == id {
			return r
		}
	}
	return rooms[0]
}

func loadConfig() (Config, error) {
	rooms, err := loadRooms()
	if err != nil {
		return Config{}, err
	}

	bbbURL, err := url.Parse(os.Getenv("BBB_SERVER_URL"))
	if err != nil {
		return Config{}, fmt.Errorf("BBB_SERVER_URL is invalid: %w", err)
	}
	if bbbURL.Scheme == "" || bbbURL.Host == "" {
		return Config{}, fmt.Errorf("BBB_SERVER_URL must be an absolute URL, e.g. https://bbb.example.com/bigbluebutton/")
	}

	return Config{
		BBBURL:    bbbURL,
		BBBSecret: os.Getenv("BBB_SERVER_SECRET"),

		Rooms:       rooms,
		FrontendURL: os.Getenv("APP_FRONTEND_URL"),

		UserPassword:      os.Getenv("APP_USER_PASSWORD"),
		ModeratorPassword: os.Getenv("APP_MODERATOR_PASSWORD"),
		ListenAddr:        getEnvDefault("APP_LISTEN_ADDR", "127.0.0.1:8080"),

		EnableRealIP: strings.EqualFold(os.Getenv("ENABLE_REAL_IP"), "true"),

		MetricsUsername: os.Getenv("METRICS_USERNAME"),
		MetricsPassword: os.Getenv("METRICS_PASSWORD"),
	}, nil
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log.SetFlags(0) // systemd and container runtimes add their own timestamps
	envPath := flag.String("env-path", "", "path to the environment file to load (default: .env in current directory)")
	flag.Parse()

	if *envPath != "" {
		if err := godotenv.Load(*envPath); err != nil {
			log.Fatalf("Error loading env file %s: %v", *envPath, err)
		}
	} else {
		_ = godotenv.Load()
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("Error: %v", err)
	}
	if cfg.BBBSecret == "" {
		log.Fatal("Error: BBB_SERVER_SECRET is not set")
	}
	if cfg.UserPassword == "" || cfg.ModeratorPassword == "" {
		log.Fatal("Error: APP_USER_PASSWORD and APP_MODERATOR_PASSWORD must both be set")
	}
	if (cfg.MetricsUsername == "") != (cfg.MetricsPassword == "") {
		log.Fatal("Error: METRICS_USERNAME and METRICS_PASSWORD must both be set or both be empty")
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// Startup connection check — warn but don't abort if BBB is unreachable.
	versionResp, err := getBBBVersion(cfg, httpClient)
	if err != nil {
		log.Printf("Warning: could not reach BBB server: %v", err)
	} else {
		log.Printf("Connected to BigBlueButton %s", versionResp.BBBVersion)
	}

	tmpl, err := template.ParseFS(staticFiles, "static/index.html")
	if err != nil {
		log.Fatalf("Error parsing template: %v", err)
	}

	srv := &server{
		config:     cfg,
		tmpl:       tmpl,
		httpClient: httpClient,
		limiter:    &rateLimiter{addrs: make(map[string]*addrState)},
	}
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "opencast_meet_ratelimit_tracked_ips",
			Help: "IPs currently tracked by the rate limiter",
		},
		func() float64 { return float64(srv.limiter.activeIPs()) },
	))

	http.HandleFunc("/", srv.handleIndex)
	http.HandleFunc("/join", srv.handleJoin)
	metricsHandler := promhttp.Handler()
	if cfg.MetricsUsername != "" {
		metricsHandler = basicAuthHandler(cfg.MetricsUsername, cfg.MetricsPassword, metricsHandler)
	}
	http.Handle("/metrics", metricsHandler)
	http.Handle("/static/", http.FileServer(http.FS(staticFiles)))

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           securityHeaders(http.DefaultServeMux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("Listening on http://%s", cfg.ListenAddr)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
