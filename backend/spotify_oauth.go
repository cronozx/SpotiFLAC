package backend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	spotifyOAuthFile     = "spotify_oauth.json"
	spotifyRedirectURI   = "http://127.0.0.1:8888/callback"
	spotifyCallbackAddr  = "127.0.0.1:8888"
	spotifyScopes        = "playlist-read-private playlist-read-collaborative user-library-read"
	spotifyAuthorizeURL  = "https://accounts.spotify.com/authorize"
	spotifyTokenURL      = "https://accounts.spotify.com/api/token"
	spotifyTokenRefreshS = 60
)

// spotifyOAuthState is the persisted credential/token file.
type spotifyOAuthState struct {
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}

var (
	spotifyOAuthMu     sync.Mutex
	spotifyOAuthClient = &http.Client{Timeout: 30 * time.Second}

	spotifyAuthServer *http.Server
	spotifyAuthState  string
)

func spotifyOAuthPath() (string, error) {
	dir, err := EnsureAppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, spotifyOAuthFile), nil
}

func loadSpotifyOAuthState() (*spotifyOAuthState, error) {
	path, err := spotifyOAuthPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &spotifyOAuthState{}, nil
		}
		return nil, err
	}

	var state spotifyOAuthState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func saveSpotifyOAuthState(state *spotifyOAuthState) error {
	path, err := spotifyOAuthPath()
	if err != nil {
		return err
	}

	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, payload, 0o600)
}

func randomHexString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// SaveSpotifyCredentials persists the user's Spotify app clientID/secret.
func SaveSpotifyCredentials(clientID, clientSecret string) error {
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	if clientID == "" || clientSecret == "" {
		return errors.New("clientID and clientSecret are required")
	}

	spotifyOAuthMu.Lock()
	defer spotifyOAuthMu.Unlock()

	state, err := loadSpotifyOAuthState()
	if err != nil {
		return err
	}

	// If credentials changed, existing tokens are no longer valid.
	if state.ClientID != clientID || state.ClientSecret != clientSecret {
		state.AccessToken = ""
		state.RefreshToken = ""
		state.ExpiresAt = 0
	}

	state.ClientID = clientID
	state.ClientSecret = clientSecret

	return saveSpotifyOAuthState(state)
}

// GetSpotifyCredentials returns the saved clientID/secret and whether a refresh
// token is present (i.e. the account is connected).
func GetSpotifyCredentials() (clientID string, clientSecret string, connected bool, err error) {
	spotifyOAuthMu.Lock()
	defer spotifyOAuthMu.Unlock()

	state, err := loadSpotifyOAuthState()
	if err != nil {
		return "", "", false, err
	}

	return state.ClientID, state.ClientSecret, state.RefreshToken != "", nil
}

// SpotifyIsConnected reports whether a refresh token is stored.
func SpotifyIsConnected() bool {
	spotifyOAuthMu.Lock()
	defer spotifyOAuthMu.Unlock()

	state, err := loadSpotifyOAuthState()
	if err != nil {
		return false
	}
	return state.RefreshToken != ""
}

// DisconnectSpotify clears stored tokens but keeps the clientID/secret.
func DisconnectSpotify() error {
	spotifyOAuthMu.Lock()
	defer spotifyOAuthMu.Unlock()

	state, err := loadSpotifyOAuthState()
	if err != nil {
		return err
	}

	state.AccessToken = ""
	state.RefreshToken = ""
	state.ExpiresAt = 0

	return saveSpotifyOAuthState(state)
}

// shutdownSpotifyAuthServerLocked stops any running callback listener.
// Callers must hold spotifyOAuthMu.
func shutdownSpotifyAuthServerLocked() {
	if spotifyAuthServer == nil {
		return
	}

	server := spotifyAuthServer
	spotifyAuthServer = nil

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

// BeginSpotifyAuth starts a local callback server and returns the Spotify
// authorize URL the caller should open in a browser.
func BeginSpotifyAuth() (authURL string, err error) {
	spotifyOAuthMu.Lock()
	defer spotifyOAuthMu.Unlock()

	state, err := loadSpotifyOAuthState()
	if err != nil {
		return "", err
	}
	if state.ClientID == "" || state.ClientSecret == "" {
		return "", errors.New("spotify credentials not set; save clientID and clientSecret first")
	}

	// Guard against a stale listener from a previous, abandoned attempt.
	shutdownSpotifyAuthServerLocked()

	oauthState, err := randomHexString(16)
	if err != nil {
		return "", err
	}
	spotifyAuthState = oauthState

	listener, err := net.Listen("tcp", spotifyCallbackAddr)
	if err != nil {
		return "", fmt.Errorf("failed to listen on %s: %w", spotifyCallbackAddr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", spotifyCallbackHandler)

	server := &http.Server{Handler: mux}
	spotifyAuthServer = server

	go func() {
		_ = server.Serve(listener)
	}()

	params := url.Values{}
	params.Set("client_id", state.ClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", spotifyRedirectURI)
	params.Set("scope", spotifyScopes)
	params.Set("state", oauthState)
	// Always show the consent screen so the user is explicitly asked to grant
	// access to their private (and collaborative) playlists, even if they have
	// authorized this app before.
	params.Set("show_dialog", "true")

	return spotifyAuthorizeURL + "?" + params.Encode(), nil
}

func spotifyCallbackHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	if authErr := query.Get("error"); authErr != "" {
		writeCallbackHTML(w, "Authorization failed: "+authErr)
		go finishSpotifyAuth()
		return
	}

	spotifyOAuthMu.Lock()
	expectedState := spotifyAuthState
	spotifyOAuthMu.Unlock()

	if query.Get("state") == "" || query.Get("state") != expectedState {
		writeCallbackHTML(w, "Authorization failed: invalid state.")
		go finishSpotifyAuth()
		return
	}

	code := query.Get("code")
	if code == "" {
		writeCallbackHTML(w, "Authorization failed: missing code.")
		go finishSpotifyAuth()
		return
	}

	if err := exchangeSpotifyCode(code); err != nil {
		writeCallbackHTML(w, "Authorization failed: "+err.Error())
		go finishSpotifyAuth()
		return
	}

	writeCallbackHTML(w, "Spotify connected. You may close this window.")
	go finishSpotifyAuth()
}

func writeCallbackHTML(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>SpotiFLAC</title></head><body style=\"font-family:sans-serif;text-align:center;padding:3rem;\"><h2>"+message+"</h2></body></html>")
}

// finishSpotifyAuth shuts down the callback server after a request completes.
func finishSpotifyAuth() {
	spotifyOAuthMu.Lock()
	defer spotifyOAuthMu.Unlock()
	spotifyAuthState = ""
	shutdownSpotifyAuthServerLocked()
}

// exchangeSpotifyCode swaps an authorization code for tokens and persists them.
func exchangeSpotifyCode(code string) error {
	spotifyOAuthMu.Lock()
	state, err := loadSpotifyOAuthState()
	clientID := ""
	clientSecret := ""
	if err == nil {
		clientID = state.ClientID
		clientSecret = state.ClientSecret
	}
	spotifyOAuthMu.Unlock()

	if err != nil {
		return err
	}
	if clientID == "" || clientSecret == "" {
		return errors.New("spotify credentials not set")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", spotifyRedirectURI)

	tokenResp, err := requestSpotifyToken(form, clientID, clientSecret)
	if err != nil {
		return err
	}

	spotifyOAuthMu.Lock()
	defer spotifyOAuthMu.Unlock()

	current, err := loadSpotifyOAuthState()
	if err != nil {
		return err
	}

	current.AccessToken = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		current.RefreshToken = tokenResp.RefreshToken
	}
	current.ExpiresAt = time.Now().Unix() + int64(tokenResp.ExpiresIn)

	return saveSpotifyOAuthState(current)
}

type spotifyTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func requestSpotifyToken(form url.Values, clientID, clientSecret string) (*spotifyTokenResponse, error) {
	req, err := http.NewRequest(http.MethodPost, spotifyTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(clientID, clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := spotifyOAuthClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spotify token request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp spotifyTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, err
	}
	if tokenResp.AccessToken == "" {
		return nil, errors.New("spotify token response missing access_token")
	}

	return &tokenResp, nil
}

// SpotifyAccessToken returns a valid access token, refreshing it if needed.
func SpotifyAccessToken() (string, error) {
	spotifyOAuthMu.Lock()
	defer spotifyOAuthMu.Unlock()

	state, err := loadSpotifyOAuthState()
	if err != nil {
		return "", err
	}
	if state.RefreshToken == "" {
		return "", errors.New("spotify not connected")
	}

	if state.AccessToken != "" && time.Now().Unix() < state.ExpiresAt-spotifyTokenRefreshS {
		return state.AccessToken, nil
	}

	if state.ClientID == "" || state.ClientSecret == "" {
		return "", errors.New("spotify credentials not set")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", state.RefreshToken)

	tokenResp, err := requestSpotifyToken(form, state.ClientID, state.ClientSecret)
	if err != nil {
		return "", err
	}

	state.AccessToken = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		state.RefreshToken = tokenResp.RefreshToken
	}
	state.ExpiresAt = time.Now().Unix() + int64(tokenResp.ExpiresIn)

	if err := saveSpotifyOAuthState(state); err != nil {
		return "", err
	}

	return state.AccessToken, nil
}
