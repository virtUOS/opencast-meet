package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/joho/godotenv"
)

// VersionResponse represents the XML response from the /bigbluebutton/api/ endpoint
type VersionResponse struct {
	XMLName        xml.Name `xml:"response"`
	ReturnCode     string   `xml:"returncode"`
	Version        string   `xml:"version"`
	APIVersion     string   `xml:"apiVersion"`
	BBBVersion     string   `xml:"bbbVersion"`
	HTML5PluginSdk string   `xml:"html5PluginSdkVersion"`
	GraphQLWsUrl   string   `xml:"graphqlWebsocketUrl"`
	GraphQLApiUrl  string   `xml:"graphqlApiUrl"`
}

// CreateResponse represents the XML response from the create API endpoint
type CreateResponse struct {
	XMLName       xml.Name `xml:"response"`
	ReturnCode    string   `xml:"returncode"`
	Message       string   `xml:"message"`
	MessageKey    string   `xml:"messageKey"`
	MeetingID     string   `xml:"meetingID"`
	InternalID    string   `xml:"internalMeetingID"`
	AttendeePW    string   `xml:"attendeePW"`
	ModeratorPW   string   `xml:"moderatorPW"`
	CreateTime    string   `xml:"createTime"`
	VoiceBridge   string   `xml:"voiceBridge"`
	DialNumber    string   `xml:"dialNumber"`
	CreateTime2   string   `xml:"createDate"`
	HasUserJoined string   `xml:"hasUserJoined"`
	Duration      string   `xml:"duration"`
	HasEnded      string   `xml:"hasBeenForciblyEnded"`
}

// CalculateChecksum generates the SHA-256 checksum for BigBlueButton API calls
// The checksum is calculated as: SHA256(callName + queryString + secret)
func calculateChecksum(callName string, queryString string, secret string) string {
	raw := callName + queryString + secret
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

// getBBBVersion fetches the BigBlueButton server version
func getBBBVersion(baseURL string) (*VersionResponse, error) {
	// Build the request URL
	apiURL, err := url.JoinPath(baseURL, "api")
	if err != nil {
		return nil, fmt.Errorf("failed to build API URL: %w", err)
	}

	resp, err := http.Get(apiURL)
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

// createMeeting creates a new meeting room on the BigBlueButton server
func createMeeting(baseURL, secret string) (*CreateResponse, error) {
	// Demo parameters for creating a meeting
	meetingName := "Demo Meeting"
	meetingID := "demo-meeting-" + fmt.Sprintf("%d", os.Getpid()) + "-" + fmt.Sprintf("%d", len("Demo Meeting"))
	attendeePW := "ap"
	moderatorPW := "mp"

	// Build query string (URL encoded)
	params := url.Values{}
	params.Add("name", meetingName)
	params.Add("meetingID", meetingID)
	params.Add("attendeePW", attendeePW)
	params.Add("moderatorPW", moderatorPW)

	// Calculate checksum
	checksum := calculateChecksum("create", params.Encode(), secret)
	params.Add("checksum", checksum)

	// Build the request URL
	serverUrl, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to build API URL: %w", err)
	}
	apiURL := serverUrl.JoinPath("api", "create")
	apiURL.RawQuery = params.Encode()

	resp, err := http.Get(apiURL.String())
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

func main() {
	// Optionally load .env file (silently ignore if not found)
	_ = godotenv.Load()

	// Read environment variables
	bbbURL := os.Getenv("BBB_SERVER_URL")
	bbbSecret := os.Getenv("BBB_SERVER_SECRET")

	if bbbURL == "" {
		fmt.Fprintln(os.Stderr, "Error: BBB_SERVER_URL environment variable is not set")
		os.Exit(1)
	}

	if bbbSecret == "" {
		fmt.Fprintln(os.Stderr, "Error: BBB_SERVER_SECRET environment variable is not set")
		os.Exit(1)
	}

	// Get and print the BBB version
	fmt.Println("=== Getting BigBlueButton Version ===")
	versionResp, err := getBBBVersion(bbbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting version: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("BigBlueButton Version: %s\n", versionResp.BBBVersion)
	fmt.Println()

	// Create a meeting
	fmt.Println("=== Creating Meeting ===")
	createResp, err := createMeeting(bbbURL, bbbSecret)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating meeting: %v\n", err)
		os.Exit(1)
	}

	// Print the create response
	fmt.Printf("Return Code: %s\n", createResp.ReturnCode)
	fmt.Printf("Message: %s\n", createResp.Message)
	fmt.Printf("Message Key: %s\n", createResp.MessageKey)
	fmt.Printf("Meeting ID: %s\n", createResp.MeetingID)
	fmt.Printf("Internal ID: %s\n", createResp.InternalID)
	fmt.Printf("Attendee Password: %s\n", createResp.AttendeePW)
	fmt.Printf("Moderator Password: %s\n", createResp.ModeratorPW)
	fmt.Printf("Voice Bridge: %s\n", createResp.VoiceBridge)
	fmt.Printf("Dial Number: %s\n", createResp.DialNumber)
}
