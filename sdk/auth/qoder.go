package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// qoderRefreshLead is the duration before token expiry when refresh should occur.
var qoderRefreshLead = 5 * time.Minute

// QoderAuthenticator implements the PKCE + URI-scheme login for the Qoder provider.
type QoderAuthenticator struct{}

// NewQoderAuthenticator constructs a new authenticator instance.
func NewQoderAuthenticator() Authenticator { return &QoderAuthenticator{} }

// Provider returns the provider key for qoder.
func (QoderAuthenticator) Provider() string { return "qoder" }

// RefreshLead instructs the manager to refresh five minutes before expiry.
func (QoderAuthenticator) RefreshLead() *time.Duration {
	return &qoderRefreshLead
}

// qoderCallbackResult holds the parsed callback data.
type qoderCallbackResult struct {
	TokenString string
	AuthField   string
	Error       string
}

// Login launches a local HTTP server to catch the qoder:// URI callback,
// opens the browser for Qoder login, and waits for the token.
func (a QoderAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	callbackPort := qoder.CallbackPort
	if opts.CallbackPort > 0 {
		callbackPort = opts.CallbackPort
	}

	// Generate PKCE + machine ID
	nonce, challenge, verifier, err := qoder.GeneratePKCE()
	if err != nil {
		return nil, fmt.Errorf("qoder: %w", err)
	}

	machineID := qoder.GenerateMachineID("cliproxy", "00:00:00:00:00:00", "server", "x86_64")
	_ = verifier // stored in metadata for potential future token exchange

	callbackState, errState := qoder.GenerateCallbackState()
	if errState != nil {
		return nil, fmt.Errorf("qoder: %w", errState)
	}

	// Start local callback server bound to this login attempt.
	srv, port, cbChan, errServer := startQoderCallbackServer(callbackPort, callbackState, nonce)
	if errServer != nil {
		return nil, fmt.Errorf("qoder: failed to start callback server: %w", errServer)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	_ = port // port is used for the callback URL

	// Register qoder:// URI protocol handler (Windows: registry + VBS, other: no-op)
	// Do not register the qoder:// protocol handler automatically: installing an
	// OS-level URI handler is an explicit user action. If the browser cannot
	// return to this terminal, the manual callback prompt below still works.

	authURL := qoder.BuildAuthURL(nonce, challenge, machineID)

	challengeDelivered := NotifyLoginChallenge(opts, LoginChallenge{URL: authURL})
	if !challengeDelivered {
		if !opts.NoBrowser {
			fmt.Println("Opening browser for Qoder authentication")
			if !browser.IsAvailable() {
				log.Warn("No browser available; please open the URL manually")
				util.PrintSSHTunnelInstructions(port)
				fmt.Printf("Visit the following URL to continue authentication:\n%s\n", authURL)
			} else if errOpen := browser.OpenURL(authURL); errOpen != nil {
				log.Warnf("Failed to open browser automatically: %v", errOpen)
				util.PrintSSHTunnelInstructions(port)
				fmt.Printf("Visit the following URL to continue authentication:\n%s\n", authURL)
			}
		} else {
			util.PrintSSHTunnelInstructions(port)
			fmt.Printf("Visit the following URL to continue authentication:\n%s\n", authURL)
		}
	}

	fmt.Println("Waiting for Qoder authentication callback...")

	var cbRes qoderCallbackResult
	timeoutTimer := time.NewTimer(5 * time.Minute)
	defer timeoutTimer.Stop()

	var manualPromptTimer *time.Timer
	var manualPromptC <-chan time.Time
	if opts.Prompt != nil {
		manualPromptTimer = time.NewTimer(15 * time.Second)
		manualPromptC = manualPromptTimer.C
		defer manualPromptTimer.Stop()
	}

	var manualInputCh <-chan string
	var manualInputErrCh <-chan error

waitForCallback:
	for {
		select {
		case res := <-cbChan:
			cbRes = res
			break waitForCallback
		case <-manualPromptC:
			manualPromptC = nil
			if manualPromptTimer != nil {
				manualPromptTimer.Stop()
			}
			select {
			case res := <-cbChan:
				cbRes = res
				break waitForCallback
			default:
			}
			manualInputCh, manualInputErrCh = misc.AsyncPrompt(opts.Prompt, "Paste the Qoder callback URL (or press Enter to keep waiting): ")
			continue
		case input := <-manualInputCh:
			manualInputCh = nil
			manualInputErrCh = nil
			input = strings.TrimSpace(input)
			if input == "" {
				continue
			}
			// Try to parse as qoder:// URL or a bare callback query string.
			if res, ok := parseQoderCallbackURL(input, nonce, callbackState); ok {
				cbRes = res
				break waitForCallback
			}
			continue
		case errManual := <-manualInputErrCh:
			return nil, errManual
		case <-timeoutTimer.C:
			return nil, fmt.Errorf("qoder: authentication timed out")
		}
	}

	if cbRes.Error != "" {
		return nil, fmt.Errorf("qoder: authentication failed: %s", cbRes.Error)
	}
	if cbRes.TokenString == "" {
		return nil, fmt.Errorf("qoder: missing token in callback")
	}

	// Decode auth field to get UID
	uid := ""
	name := ""
	email := ""
	if cbRes.AuthField != "" {
		authInfo, errDecode := qoder.DecodeAuthField(cbRes.AuthField)
		if errDecode != nil {
			log.Warnf("qoder: failed to decode auth field: %v", errDecode)
		} else {
			if v, ok := authInfo["uid"].(string); ok {
				uid = v
			}
			if v, ok := authInfo["name"].(string); ok {
				name = v
			}
		}
	}

	// If UID not found via auth field, try the user status endpoint
	if uid == "" {
		authSvc := qoder.NewQoderAuth(nil)
		user, errUser := authSvc.FetchUserStatus(cbRes.TokenString)
		if errUser != nil {
			log.Warnf("qoder: user status probe failed: %v", errUser)
		} else {
			uid = user.ID
			name = user.Name
			email = user.Email
		}
	}

	if uid == "" {
		// Fallback: derive a stable UID from the token hash so we can still save credentials
		tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(cbRes.TokenString)))
		uid = tokenHash[:16]
		log.Warnf("qoder: using derived UID from token hash: %s", uid)
	}

	now := time.Now()
	metadata := map[string]any{
		"type":         "qoder",
		"access_token": cbRes.TokenString,
		"auth":         cbRes.AuthField,
		"nonce":        nonce,
		"verifier":     verifier,
		"machine_id":   machineID,
		"uid":          uid,
		"timestamp":    now.UnixMilli(),
	}
	if name != "" {
		metadata["name"] = name
	}
	if email != "" {
		metadata["email"] = email
	}

	fileName := qoder.CredentialFileName(uid)
	label := name
	if label == "" {
		label = uid
	}
	if label == "" {
		label = "qoder"
	}

	fmt.Println("Qoder authentication successful")
	return &coreauth.Auth{
		ID:       fileName,
		Provider: "qoder",
		FileName: fileName,
		Label:    label,
		Metadata: metadata,
	}, nil
}

func startQoderCallbackServer(port int, callbackState, expectedNonce string) (*http.Server, int, <-chan qoderCallbackResult, error) {
	if port <= 0 {
		port = qoder.CallbackPort
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, 0, nil, err
	}
	port = listener.Addr().(*net.TCPAddr).Port
	resultCh := make(chan qoderCallbackResult, 1)

	mux := http.NewServeMux()
	// Only the per-login callback path is served: a page that does not know the
	// random state cannot inject a token into this login.
	mux.HandleFunc("/forward/"+callbackState, func(w http.ResponseWriter, r *http.Request) {
		rawURL := r.URL.Query().Get("url")
		// Match Python: raw_url = unquote(raw_url) — VBS double-encodes the URL
		rawURL, _ = url.QueryUnescape(rawURL)
		if res, ok := parseQoderCallbackURL(rawURL, expectedNonce, callbackState); ok {
			select {
			case resultCh <- res:
			default:
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>Qoder Login</title></head>` +
			`<body style="display:flex;justify-content:center;align-items:center;height:100vh;` +
			`font-family:system-ui;background:#1a1a2e;color:#e0e0e0">` +
			`<div style="text-align:center">` +
			`<h1 style="color:#4CAF50">&#10003; Login Successful</h1>` +
			`<p>You can close this window and return to the terminal.</p>` +
			`</div></body></html>`))
	})

	srv := &http.Server{Handler: mux}
	go func() {
		if errServe := srv.Serve(listener); errServe != nil && !strings.Contains(errServe.Error(), "Server closed") {
			log.Warnf("qoder callback server error: %v", errServe)
		}
	}()

	return srv, port, resultCh, nil
}

// parseQoderCallbackURL accepts a qoder:// callback URL (or a bare query
// string pasted by the user) and returns the token when it belongs to this
// login attempt. When the callback carries a nonce or state parameter, it must
// match the values generated for this login; callbacks that omit both are
// accepted for provider flows that do not echo them.
func parseQoderCallbackURL(raw string, expectedNonce, expectedState string) (qoderCallbackResult, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return qoderCallbackResult{}, false
	}
	const prefix = "qoder://aicoding.aicoding-agent/login-success"
	qs := raw
	if strings.HasPrefix(raw, prefix) {
		qs = strings.TrimPrefix(raw, prefix)
		qs = strings.TrimPrefix(qs, "?")
	} else if strings.Contains(raw, "tokenString=") || strings.Contains(raw, "token=") {
		if idx := strings.Index(raw, "?"); idx >= 0 {
			qs = raw[idx+1:]
		}
	} else {
		return qoderCallbackResult{}, false
	}

	token, authField := extractQoderCallbackParams(qs)
	if token == "" {
		return qoderCallbackResult{}, false
	}

	if params, errParse := url.ParseQuery(qs); errParse == nil {
		if got := params.Get("nonce"); got != "" && expectedNonce != "" &&
			subtle.ConstantTimeCompare([]byte(got), []byte(expectedNonce)) != 1 {
			log.Warn("qoder: callback rejected: nonce mismatch")
			return qoderCallbackResult{}, false
		}
		if got := params.Get("state"); got != "" && expectedState != "" &&
			subtle.ConstantTimeCompare([]byte(got), []byte(expectedState)) != 1 {
			log.Warn("qoder: callback rejected: state mismatch")
			return qoderCallbackResult{}, false
		}
	}

	return qoderCallbackResult{TokenString: token, AuthField: authField}, true
}

// extractQoderCallbackParams extracts tokenString and auth from the Qoder callback
// query string. The query string comes from a qoder:// URI where parameter values
// contain URL-encoded special characters (like %26 for &). Standard url.ParseQuery
// would incorrectly interpret encoded %26 as a parameter separator after URL decoding,
// so we extract the raw parameter values by finding known key= prefixes and splitting
// on key boundaries, then URL-decode each value individually.
func extractQoderCallbackParams(qs string) (token, authField string) {
	// Known parameter keys in the callback URL
	params := map[string]string{}
	keys := []string{"tokenString", "token", "auth"}

	for _, key := range keys {
		prefix := key + "="
		idx := strings.Index(qs, prefix)
		if idx < 0 {
			continue
		}
		valueStart := idx + len(prefix)
		// Find the end of this parameter: look for the next "&key=" boundary
		rest := qs[valueStart:]
		endIdx := len(rest)
		for _, nextKey := range keys {
			boundary := "&" + nextKey + "="
			if bi := strings.Index(rest, boundary); bi >= 0 && bi < endIdx {
				endIdx = bi
			}
		}
		rawValue := rest[:endIdx]
		// URL-decode the value
		decoded, err := url.QueryUnescape(rawValue)
		if err != nil {
			decoded = rawValue
		}
		params[key] = decoded
	}

	// Try tokenString first, fallback to token
	token = params["tokenString"]
	if token == "" {
		token = params["token"]
	}
	authField = params["auth"]
	return token, authField
}
