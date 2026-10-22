package gitlab

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBaseURL = "https://gitlab.com"

	DefaultCallbackPort = 17171

	defaultOAuthScope = "api read_user"
)

type PKCECodes struct {
	CodeVerifier string

	CodeChallenge string
}

type OAuthResult struct {
	Code string

	State string

	Error string
}

type OAuthServer struct {
	server     *http.Server
	port       int
	resultChan chan *OAuthResult
	errorChan  chan error
	mu         sync.Mutex
	running    bool
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	CreatedAt    int64  `json:"created_at"`
	ExpiresIn    int    `json:"expires_in"`
}

type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	PublicEmail string `json:"public_email"`
}

type PersonalAccessTokenSelf struct {
	ID     int64    `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	UserID int64    `json:"user_id"`
}

type ModelDetails struct {
	ModelProvider string `json:"model_provider"`

	ModelName string `json:"model_name"`
}

type DirectAccessResponse struct {
	BaseURL      string            `json:"base_url"`
	Token        string            `json:"token"`
	ExpiresAt    int64             `json:"expires_at"`
	Headers      map[string]string `json:"headers"`
	ModelDetails *ModelDetails     `json:"model_details,omitempty"`
}

type DiscoveredModel struct {
	ModelProvider string

	ModelName string
}

type AuthClient struct {
	httpClient *http.Client
}

// NewAuthClientWithHTTPClient returns an AuthClient backed by httpClient. The
// redirect policy is applied when httpClient is nil; callers that inject their
// own client are responsible for its transport and redirect policy.
func NewAuthClientWithHTTPClient(httpClient *http.Client) *AuthClient {
	if httpClient == nil {
		httpClient = &http.Client{CheckRedirect: gitLabCheckRedirect}
	}
	return &AuthClient{httpClient: httpClient}
}
func NewAuthClient(cfg *config.Config) *AuthClient {
	client := &http.Client{CheckRedirect: gitLabCheckRedirect}
	if cfg != nil {
		client = util.SetProxy(&cfg.SDKConfig, client)
	}
	return &AuthClient{httpClient: client}
}

const gitLabMaxRedirects = 5

// gitLabCheckRedirect keeps credentials bound to the configured origin: only
// HTTPS redirects on the exact same scheme://host:port are followed.
func gitLabCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= gitLabMaxRedirects {
		return fmt.Errorf("gitlab: stopped after %d redirects", len(via))
	}
	if req.URL == nil {
		return fmt.Errorf("gitlab: refusing redirect with an empty URL")
	}
	if req.URL.User != nil {
		return fmt.Errorf("gitlab: refusing redirect URL with userinfo")
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("gitlab: refusing redirect to non-https URL %q", req.URL.Redacted())
	}
	if len(via) == 0 || via[0].URL == nil {
		return nil
	}
	if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("gitlab: refusing redirect to different origin %q", req.URL.Redacted())
	}
	return nil
}

// ValidateBaseURL returns the normalized GitLab origin after enforcing HTTPS.
// Private/self-hosted instances are supported as long as they serve HTTPS.
func ValidateBaseURL(raw string) (string, error) {
	normalized := NormalizeBaseURL(raw)
	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", fmt.Errorf("gitlab base URL is invalid: %w", err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("gitlab base URL must use https: %q", normalized)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("gitlab base URL must include a host: %q", normalized)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("gitlab base URL must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("gitlab base URL must not contain a query or fragment")
	}
	return normalized, nil
}
func NormalizeBaseURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return DefaultBaseURL
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	value = strings.TrimRight(value, "/")
	return value
}
func TokenExpiry(now time.Time, token *TokenResponse) time.Time {
	if token == nil {
		return time.Time{}
	}
	if token.CreatedAt > 0 && token.ExpiresIn > 0 {
		return time.Unix(token.CreatedAt+int64(token.ExpiresIn), 0).UTC()
	}
	if token.ExpiresIn > 0 {
		return now.UTC().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	return time.Time{}
}
func GeneratePKCECodes() (*PKCECodes, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("gitlab pkce generation failed: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return &PKCECodes{
		CodeVerifier:  verifier,
		CodeChallenge: challenge,
	}, nil
}
func NewOAuthServer(port int) *OAuthServer {
	return &OAuthServer{
		port:       port,
		resultChan: make(chan *OAuthResult, 1),
		errorChan:  make(chan error, 1),
	}
}
func (s *OAuthServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return fmt.Errorf("gitlab oauth server already running")
	}
	if !s.isPortAvailable() {
		return fmt.Errorf("port %d is already in use", s.port)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", s.handleCallback)
	s.server = &http.Server{
		Addr:         loopbackAddr(s.port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	s.running = true
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.errorChan <- err
		}
	}()
	time.Sleep(100 * time.Millisecond)
	return nil
}
func (s *OAuthServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.server == nil {
		return nil
	}
	defer func() {
		s.running = false
		s.server = nil
	}()
	return s.server.Shutdown(ctx)
}
func (s *OAuthServer) WaitForCallback(timeout time.Duration) (*OAuthResult, error) {
	return s.WaitForCallbackContext(context.Background(), timeout)
}

// WaitForCallbackContext waits for the callback while honoring caller
// cancellation, so an abandoned login does not keep a waiter goroutine alive.
func (s *OAuthServer) WaitForCallbackContext(ctx context.Context, timeout time.Duration) (*OAuthResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-s.resultChan:
		return result, nil
	case err := <-s.errorChan:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("timeout waiting for OAuth callback")
	}
}
func (s *OAuthServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	if errParam := strings.TrimSpace(query.Get("error")); errParam != "" {
		s.sendResult(&OAuthResult{Error: errParam})
		http.Error(w, errParam, http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(query.Get("code"))
	state := strings.TrimSpace(query.Get("state"))
	if code == "" || state == "" {
		s.sendResult(&OAuthResult{Error: "missing_code_or_state"})
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}
	s.sendResult(&OAuthResult{Code: code, State: state})
	_, _ = w.Write([]byte("GitLab authentication received. You can close this tab."))
}
func (s *OAuthServer) sendResult(result *OAuthResult) {
	select {
	case s.resultChan <- result:
	default:
		log.Debug("gitlab oauth result channel full, dropping callback result")
	}
}
func (s *OAuthServer) isPortAvailable() bool {
	listener, err := net.Listen("tcp", loopbackAddr(s.port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}
func RedirectURL(port int) string {
	return fmt.Sprintf("http://localhost:%d/auth/callback", port)
}
func (c *AuthClient) GenerateAuthURL(baseURL, clientID, redirectURI, state string, pkce *PKCECodes) (string, error) {
	if pkce == nil {
		return "", fmt.Errorf("gitlab auth URL generation failed: PKCE codes are required")
	}
	if strings.TrimSpace(clientID) == "" {
		return "", fmt.Errorf("gitlab auth URL generation failed: client ID is required")
	}
	validatedBaseURL, err := ValidateBaseURL(baseURL)
	if err != nil {
		return "", err
	}
	baseURL = validatedBaseURL
	params := url.Values{
		"client_id":             {strings.TrimSpace(clientID)},
		"response_type":         {"code"},
		"redirect_uri":          {strings.TrimSpace(redirectURI)},
		"scope":                 {defaultOAuthScope},
		"state":                 {strings.TrimSpace(state)},
		"code_challenge":        {pkce.CodeChallenge},
		"code_challenge_method": {"S256"},
	}
	return fmt.Sprintf("%s/oauth/authorize?%s", baseURL, params.Encode()), nil
}
func (c *AuthClient) ExchangeCodeForTokens(ctx context.Context, baseURL, clientID, clientSecret, redirectURI, code, codeVerifier string) (*TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {strings.TrimSpace(clientID)},
		"code":          {strings.TrimSpace(code)},
		"redirect_uri":  {strings.TrimSpace(redirectURI)},
		"code_verifier": {strings.TrimSpace(codeVerifier)},
	}
	if secret := strings.TrimSpace(clientSecret); secret != "" {
		form.Set("client_secret", secret)
	}
	base, err := ValidateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return c.postToken(ctx, base+"/oauth/token", form)
}
func (c *AuthClient) RefreshTokens(ctx context.Context, baseURL, clientID, clientSecret, refreshToken string) (*TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {strings.TrimSpace(refreshToken)},
	}
	if clientID = strings.TrimSpace(clientID); clientID != "" {
		form.Set("client_id", clientID)
	}
	if secret := strings.TrimSpace(clientSecret); secret != "" {
		form.Set("client_secret", secret)
	}
	base, err := ValidateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return c.postToken(ctx, base+"/oauth/token", form)
}
func (c *AuthClient) postToken(ctx context.Context, tokenURL string, form url.Values) (*TokenResponse, error) {
	parsedTokenURL, parseErr := url.Parse(tokenURL)
	if parseErr != nil || parsedTokenURL.Scheme != "https" || parsedTokenURL.Host == "" {
		return nil, fmt.Errorf("gitlab token request requires an https URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("gitlab token request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gitlab token response read failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitlab token request failed with status %d: %s", resp.StatusCode, redactResponseBodyForLog(body))
	}
	var token TokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("gitlab token response decode failed: %w", err)
	}
	return &token, nil
}
func (c *AuthClient) GetCurrentUser(ctx context.Context, baseURL, token string) (*User, error) {
	base, err := ValidateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v4/user", nil)
	if err != nil {
		return nil, fmt.Errorf("gitlab user request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab user request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gitlab user response read failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitlab user request failed with status %d: %s", resp.StatusCode, redactResponseBodyForLog(body))
	}
	var user User
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("gitlab user response decode failed: %w", err)
	}
	return &user, nil
}
func (c *AuthClient) GetPersonalAccessTokenSelf(ctx context.Context, baseURL, token string) (*PersonalAccessTokenSelf, error) {
	base, err := ValidateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v4/personal_access_tokens/self", nil)
	if err != nil {
		return nil, fmt.Errorf("gitlab PAT self request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab PAT self request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gitlab PAT self response read failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitlab PAT self request failed with status %d: %s", resp.StatusCode, redactResponseBodyForLog(body))
	}
	var pat PersonalAccessTokenSelf
	if err := json.Unmarshal(body, &pat); err != nil {
		return nil, fmt.Errorf("gitlab PAT self response decode failed: %w", err)
	}
	return &pat, nil
}
func (c *AuthClient) FetchDirectAccess(ctx context.Context, baseURL, token string) (*DirectAccessResponse, error) {
	base, err := ValidateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v4/code_suggestions/direct_access", nil)
	if err != nil {
		return nil, fmt.Errorf("gitlab direct access request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab direct access request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gitlab direct access response read failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitlab direct access request failed with status %d: %s", resp.StatusCode, redactResponseBodyForLog(body))
	}
	var direct DirectAccessResponse
	if err := json.Unmarshal(body, &direct); err != nil {
		return nil, fmt.Errorf("gitlab direct access response decode failed: %w", err)
	}
	if direct.Headers == nil {
		direct.Headers = make(map[string]string)
	}
	return &direct, nil
}
func ExtractDiscoveredModels(metadata map[string]any) []DiscoveredModel {
	if len(metadata) == 0 {
		return nil
	}
	models := make([]DiscoveredModel, 0, 4)
	seen := make(map[string]struct{})
	appendModel := func(provider, name string) {
		provider = strings.TrimSpace(provider)
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		models = append(models, DiscoveredModel{
			ModelProvider: provider,
			ModelName:     name,
		})
	}
	if raw, ok := metadata["model_details"]; ok {
		appendDiscoveredModels(raw, appendModel)
	}
	appendModel(stringValue(metadata["model_provider"]), stringValue(metadata["model_name"]))
	for _, key := range []string{"models", "supported_models", "discovered_models"} {
		if raw, ok := metadata[key]; ok {
			appendDiscoveredModels(raw, appendModel)
		}
	}
	return models
}
func appendDiscoveredModels(raw any, appendModel func(provider, name string)) {
	switch typed := raw.(type) {
	case map[string]any:
		appendModel(stringValue(typed["model_provider"]), stringValue(typed["model_name"]))
		appendModel(stringValue(typed["provider"]), stringValue(typed["name"]))
		if nested, ok := typed["models"]; ok {
			appendDiscoveredModels(nested, appendModel)
		}
	case []any:
		for _, item := range typed {
			appendDiscoveredModels(item, appendModel)
		}
	case []string:
		for _, item := range typed {
			appendModel("", item)
		}
	case string:
		appendModel("", typed)
	}
}
func stringValue(raw any) string {
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	case json.Number:
		return typed.String()
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return ""
	}
}

// loopbackAddr returns the callback server address scoped to the loopback
// interface so the OAuth callback is not reachable from the network.
func loopbackAddr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}
