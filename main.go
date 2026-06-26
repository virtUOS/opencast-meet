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
	"io"
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

//go:embed static/index.html static/img
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
}

// Room is a single BBB meeting room entry.
type Room struct {
	ID                      string
	Name                    string
	Record string
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
	BBBURL    string
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
		Name: "opencast_join_attempts_total",
		Help: "Total join form submissions",
	})
	metricJoinInvalidPw = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_join_invalid_password_total",
		Help: "Join attempts with invalid password",
	})
	metricJoinModerator = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_join_moderator_total",
		Help: "Logins with moderator credential",
	})
	metricJoinViewer = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_join_viewer_total",
		Help: "Logins with viewer credential",
	})
	metricBBBErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_bbb_errors_total",
		Help: "Failures from BigBlueButton API calls",
	})
	metricBBBJoinURLErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_bbb_join_url_errors_total",
		Help: "Failures building BBB join URL",
	})
	metricJoinRateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencast_join_rate_limited_total",
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
		metricBBBJoinURLErrors,
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

// calculateChecksum generates the SHA-256 checksum for BigBlueButton API calls.
// Format: SHA256(callName + queryString + secret)
func calculateChecksum(callName, queryString, secret string) string {
	raw := callName + queryString + secret
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

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

// clientIP returns the client's IP address. When EnableRealIP is set it checks
// trusted proxy headers before falling back to r.RemoteAddr.
func (s *server) clientIP(r *http.Request) string {
	if s.config.EnableRealIP {
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return strings.TrimSpace(v)
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return strings.TrimSpace(strings.SplitN(v, ",", 2)[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// getBBBVersion fetches the BigBlueButton server version.
func getBBBVersion(baseURL string, client *http.Client) (*VersionResponse, error) {
	apiURL, err := url.JoinPath(baseURL, "api")
	if err != nil {
		return nil, fmt.Errorf("failed to build API URL: %w", err)
	}

	resp, err := client.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var versionResp VersionResponse
	if err := xml.Unmarshal(body, &versionResp); err != nil {
		return nil, fmt.Errorf("failed to parse XML response: %w", err)
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
	if room.Record != "" {
		params.Add("record", room.Record)
		params.Add("allowStartStopRecording", room.Record)
	}
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

	checksum := calculateChecksum("create", params.Encode(), cfg.BBBSecret)
	params.Add("checksum", checksum)

	serverURL, err := url.Parse(cfg.BBBURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse BBB URL: %w", err)
	}
	apiURL := serverURL.JoinPath("api", "create")
	apiURL.RawQuery = params.Encode()

	resp, err := client.Get(apiURL.String())
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var createResp CreateResponse
	if err := xml.Unmarshal(body, &createResp); err != nil {
		return nil, fmt.Errorf("failed to parse XML response: %w", err)
	}

	return &createResp, nil
}

// generateJoinURL builds a BBB join URL for the given participant name and role.
func generateJoinURL(baseURL, meetingID, fullName, role, secret string) (string, error) {
	params := url.Values{}
	params.Add("fullName", fullName)
	params.Add("meetingID", meetingID)
	params.Add("role", role)

	checksum := calculateChecksum("join", params.Encode(), secret)
	params.Add("checksum", checksum)

	serverURL, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse BBB URL: %w", err)
	}
	joinURL := serverURL.JoinPath("api", "join")
	joinURL.RawQuery = params.Encode()

	return joinURL.String(), nil
}

type indexTemplateData struct {
	Error        string
	Name         string
	Rooms        []Room
	SelectedRoom string
}

func (s *server) indexData(errMsg, name, selectedRoom string) indexTemplateData {
	return indexTemplateData{
		Error:        errMsg,
		Name:         name,
		Rooms:        s.config.Rooms,
		SelectedRoom: selectedRoom,
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

	name := r.FormValue("name")
	password := r.FormValue("password")
	roomID := r.FormValue("room")
	ip := s.clientIP(r)

	room := findRoom(s.config.Rooms, roomID)

	renderError := func(msg string) {
		redirectURL := "/?error=" + url.QueryEscape(msg)
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
		_ = s.tmpl.Execute(w, s.indexData("Too many attempts. Please wait a moment and try again.", name, room.ID))
		return
	}

	var role string
	switch password {
	case s.config.ModeratorPassword:
		role = "MODERATOR"
		metricJoinModerator.Inc()
	case s.config.UserPassword:
		role = "VIEWER"
		metricJoinViewer.Inc()
	default:
		s.limiter.recordFailure(ip)
		metricJoinInvalidPw.Inc()
		renderError("Invalid password. Please try again.")
		return
	}
	s.limiter.reset(ip)

	createResp, err := createMeeting(s.config, room, s.httpClient)
	if err != nil {
		log.Printf("error creating meeting: %v", err)
		metricBBBErrors.Inc()
		renderError("Could not connect to the meeting server. Please try again later.")
		return
	}
	if createResp.ReturnCode != "SUCCESS" {
		log.Printf("BBB create returned non-success: %s / %s", createResp.MessageKey, createResp.Message)
		metricBBBErrors.Inc()
		renderError("The meeting server returned an error. Please try again later.")
		return
	}

	joinURL, err := generateJoinURL(s.config.BBBURL, createResp.MeetingID, name, role, s.config.BBBSecret)
	if err != nil {
		log.Printf("error generating join URL: %v", err)
		metricBBBJoinURLErrors.Inc()
		renderError("Could not generate a join link. Please try again later.")
		return
	}

	http.Redirect(w, r, joinURL, http.StatusFound)
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
	for i := 1; ; i++ {
		id := os.Getenv(fmt.Sprintf("ROOM_%d_ID", i))
		if id == "" {
			break
		}
		rooms = append(rooms, Room{
			ID:                      id,
			Name:                    os.Getenv(fmt.Sprintf("ROOM_%d_NAME", i)),
			Record: getEnvDefault(fmt.Sprintf("ROOM_%d_RECORD", i), "false"),
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

	return Config{
		BBBURL:    os.Getenv("BBB_SERVER_URL"),
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
	envPath := flag.String("env-path", "", "path to the environment file to load (default: .env in current directory)")
	flag.Parse()

	if *envPath != "" {
		if err := godotenv.Load(*envPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error loading env file %s: %v\n", *envPath, err)
			os.Exit(1)
		}
	} else {
		_ = godotenv.Load()
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if cfg.BBBURL == "" {
		fmt.Fprintln(os.Stderr, "Error: BBB_SERVER_URL is not set")
		os.Exit(1)
	}
	if cfg.BBBSecret == "" {
		fmt.Fprintln(os.Stderr, "Error: BBB_SERVER_SECRET is not set")
		os.Exit(1)
	}
	if cfg.UserPassword == "" || cfg.ModeratorPassword == "" {
		fmt.Fprintln(os.Stderr, "Error: APP_USER_PASSWORD and APP_MODERATOR_PASSWORD must both be set")
		os.Exit(1)
	}
	if (cfg.MetricsUsername == "") != (cfg.MetricsPassword == "") {
		fmt.Fprintln(os.Stderr, "Error: METRICS_USERNAME and METRICS_PASSWORD must both be set or both be empty")
		os.Exit(1)
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// Startup connection check — warn but don't abort if BBB is unreachable.
	versionResp, err := getBBBVersion(cfg.BBBURL, httpClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not reach BBB server: %v\n", err)
	} else {
		log.Printf("Connected to BigBlueButton %s", versionResp.BBBVersion)
	}

	tmplData, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading template: %v\n", err)
		os.Exit(1)
	}
	tmpl, err := template.New("index").Parse(string(tmplData))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing template: %v\n", err)
		os.Exit(1)
	}

	srv := &server{
		config:     cfg,
		tmpl:       tmpl,
		httpClient: httpClient,
		limiter:    &rateLimiter{addrs: make(map[string]*addrState)},
	}
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "opencast_ratelimit_tracked_ips",
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

	log.Printf("Listening on http://%s", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
