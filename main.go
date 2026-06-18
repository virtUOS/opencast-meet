package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
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

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	BBBURL    string
	BBBSecret string

	MeetingID               string
	MeetingName             string
	MuteOnStart             string
	Record                  string
	AutoStartRecording      string
	AllowStartStopRecording string
	LoginURL                string
	LogoutURL               string
	WelcomeMessage          string
	PreUploadedPresentation string

	UserPassword      string
	ModeratorPassword string
	ListenAddr        string

	// Opencast metadata (passed as meta_* on BBB create)
	OCSeriesID      string
	OCDCCreator     string
	OCAddWebcams    string
	OCACLReadRoles  string
	OCACLWriteRoles string

	EnableRealIP bool
}

type server struct {
	config     Config
	tmpl       *template.Template
	httpClient *http.Client
	limiter    *rateLimiter
}

type rateLimiter struct {
	mu    sync.Mutex
	addrs map[string]*addrState
}

type addrState struct {
	failures int
	lastSeen time.Time
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

// recordFailure increments the failure counter for addr and returns the delay to apply.
func (rl *rateLimiter) recordFailure(addr string) time.Duration {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.evictStale()
	s := rl.addrs[addr]
	if s == nil {
		s = &addrState{}
		rl.addrs[addr] = s
	}
	s.failures++
	s.lastSeen = time.Now()
	d := time.Duration(s.failures) * rlDelayBase
	if d > rlDelayMax {
		d = rlDelayMax
	}
	return d
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

var reDate = regexp.MustCompile(`{{\s*DATE\s*}}`)

// expandPlaceholders replaces {{DATE}} (spaces optional) with today's date (YYYY-MM-DD).
func expandPlaceholders(s string) string {
	return reDate.ReplaceAllString(s, time.Now().Format("2006-01-02"))
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
func createMeeting(cfg Config, client *http.Client) (*CreateResponse, error) {
	params := url.Values{}
	params.Add("meetingID", cfg.MeetingID)
	name := expandPlaceholders(cfg.MeetingName)
	params.Add("name", name)
	if cfg.MuteOnStart != "" {
		params.Add("muteOnStart", cfg.MuteOnStart)
	}
	if cfg.Record != "" {
		params.Add("record", cfg.Record)
	}
	if cfg.AutoStartRecording != "" {
		params.Add("autoStartRecording", cfg.AutoStartRecording)
	}
	if cfg.AllowStartStopRecording != "" {
		params.Add("allowStartStopRecording", cfg.AllowStartStopRecording)
	}
	if cfg.LoginURL != "" {
		params.Add("loginURL", cfg.LoginURL)
	}
	if cfg.LogoutURL != "" {
		params.Add("logoutURL", cfg.LogoutURL)
	}
	if cfg.WelcomeMessage != "" {
		params.Add("welcome", cfg.WelcomeMessage)
	}
	if cfg.PreUploadedPresentation != "" {
		params.Add("preUploadedPresentation", cfg.PreUploadedPresentation)
	}
	if cfg.OCSeriesID != "" {
		params.Add("meta_opencast-dc-isPartOf", cfg.OCSeriesID)
		params.Add("meta_opencast-dc-title", name)
	}
	if cfg.OCDCCreator != "" {
		params.Add("meta_opencast-dc-creator", cfg.OCDCCreator)
	}
	if cfg.OCAddWebcams != "" {
		params.Add("meta_opencast-add-webcams", cfg.OCAddWebcams)
	}
	if cfg.OCACLReadRoles != "" {
		params.Add("meta_opencast-acl-read-roles", cfg.OCACLReadRoles)
	}
	if cfg.OCACLWriteRoles != "" {
		params.Add("meta_opencast-acl-write-roles", cfg.OCACLWriteRoles)
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

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.tmpl.Execute(w, struct{ Error, Name string }{
		Error: r.URL.Query().Get("error"),
		Name:  r.URL.Query().Get("name"),
	})
}

func (s *server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	name := r.FormValue("name")
	password := r.FormValue("password")

	renderError := func(msg string) {
		redirectURL := "/?error=" + url.QueryEscape(msg)
		if name != "" {
			redirectURL += "&name=" + url.QueryEscape(name)
		}
		http.Redirect(w, r, redirectURL, http.StatusSeeOther)
	}

	var role string
	switch password {
	case s.config.ModeratorPassword:
		role = "MODERATOR"
	case s.config.UserPassword:
		role = "VIEWER"
	default:
		delay := s.limiter.recordFailure(s.clientIP(r))
		time.Sleep(delay)
		renderError("Invalid password. Please try again.")
		return
	}

	createResp, err := createMeeting(s.config, s.httpClient)
	if err != nil {
		log.Printf("error creating meeting: %v", err)
		renderError("Could not connect to the meeting server. Please try again later.")
		return
	}
	if createResp.ReturnCode != "SUCCESS" {
		log.Printf("BBB create returned non-success: %s / %s", createResp.MessageKey, createResp.Message)
		renderError("The meeting server returned an error. Please try again later.")
		return
	}

	joinURL, err := generateJoinURL(s.config.BBBURL, createResp.MeetingID, name, role, s.config.BBBSecret)
	if err != nil {
		log.Printf("error generating join URL: %v", err)
		renderError("Could not generate a join link. Please try again later.")
		return
	}

	http.Redirect(w, r, joinURL, http.StatusFound)
}

func loadConfig() Config {
	return Config{
		BBBURL:    os.Getenv("BBB_SERVER_URL"),
		BBBSecret: os.Getenv("BBB_SERVER_SECRET"),

		MeetingID:               getEnvDefault("BBB_MEETING_ID", "opencast-meet"),
		MeetingName:             getEnvDefault("BBB_MEETING_NAME", "Opencast Meeting"),
		MuteOnStart:             os.Getenv("BBB_MUTE_ON_START"),
		Record:                  os.Getenv("BBB_RECORD"),
		AutoStartRecording:      os.Getenv("BBB_AUTO_START_RECORDING"),
		AllowStartStopRecording: os.Getenv("BBB_ALLOW_START_STOP_RECORDING"),
		LoginURL:                os.Getenv("BBB_LOGIN_URL"),
		LogoutURL:               os.Getenv("BBB_LOGOUT_URL"),
		WelcomeMessage:          os.Getenv("BBB_WELCOME_MESSAGE"),
		PreUploadedPresentation: os.Getenv("BBB_PRE_UPLOADED_PRESENTATION"),

		UserPassword:      os.Getenv("APP_USER_PASSWORD"),
		ModeratorPassword: os.Getenv("APP_MODERATOR_PASSWORD"),
		ListenAddr:        getEnvDefault("APP_LISTEN_ADDR", "127.0.0.1:8080"),

		OCSeriesID:      os.Getenv("OC_SERIES_ID"),
		OCDCCreator:     os.Getenv("OC_DC_CREATOR"),
		OCAddWebcams:    os.Getenv("OC_ADD_WEBCAMS"),
		OCACLReadRoles:  os.Getenv("OC_ACL_READ_ROLES"),
		OCACLWriteRoles: os.Getenv("OC_ACL_WRITE_ROLES"),

		EnableRealIP: strings.EqualFold(os.Getenv("ENABLE_REAL_IP"), "true"),
	}
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

	cfg := loadConfig()

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

	http.HandleFunc("/", srv.handleIndex)
	http.HandleFunc("/join", srv.handleJoin)
	http.Handle("/static/", http.FileServer(http.FS(staticFiles)))

	log.Printf("Listening on http://%s", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
