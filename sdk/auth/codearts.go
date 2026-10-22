package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codearts"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var codeartsRefreshLead = 4 * time.Hour

type CodeArtsAuthenticator struct{}

func NewCodeArtsAuthenticator() Authenticator { return &CodeArtsAuthenticator{} }

func (CodeArtsAuthenticator) Provider() string { return "codearts" }

func (CodeArtsAuthenticator) RefreshLead() *time.Duration {
	return &codeartsRefreshLead
}

type codeartsCallbackResult struct {
	Identifier string
	Redirect   string
	Error      string
}

// handleCodeArtsCallbackForTicket records the provider redirect received on the
// local callback listener for the login attempt identified by ticketID. The
// provider echoes ticket_id inside the redirect URL, so a local request that
// does not carry the ticket generated for this attempt is rejected instead of
// being allowed to race the genuine callback with an attacker-controlled
// identifier.
func handleCodeArtsCallbackForTicket(w http.ResponseWriter, r *http.Request, ticketID string, cbChan chan<- codeartsCallbackResult) {
	redirect := r.URL.Query().Get("redirect")
	if !codeArtsCallbackTicketMatches(redirect, ticketID) {
		select {
		case cbChan <- codeartsCallbackResult{Error: "invalid callback ticket"}:
		default:
		}
		http.Error(w, "invalid callback ticket", http.StatusForbidden)
		return
	}
	handleCodeArtsCallback(w, r, cbChan)
}

// codeArtsCallbackTicketMatches reports whether the provider redirect carries
// the ticket_id generated for this login attempt.
func codeArtsCallbackTicketMatches(redirect, ticketID string) bool {
	if redirect == "" || ticketID == "" {
		return false
	}
	parsed, err := url.Parse(redirect)
	if err != nil {
		return false
	}
	got := parsed.Query().Get("ticket_id")
	return subtle.ConstantTimeCompare([]byte(got), []byte(ticketID)) == 1
}

// parseCodeArtsCallbackURL extracts provider callback parameters from a URL
// pasted by an operator on a remote browser and applies the same per-attempt
// ticket binding as the local listener.
func parseCodeArtsCallbackURL(raw, ticketID string) (codeartsCallbackResult, error) {
	parsed, errParse := parseCallbackURLLike(raw)
	if errParse != nil {
		return codeartsCallbackResult{}, errParse
	}
	query := parsed.Query()
	identifier := query.Get("identifier")
	redirect := query.Get("redirect")
	if redirect == "" && identifier == "" {
		return codeartsCallbackResult{}, fmt.Errorf("callback URL is missing identifier and redirect parameters")
	}
	if !codeArtsCallbackTicketMatches(redirect, ticketID) {
		return codeartsCallbackResult{}, fmt.Errorf("callback ticket does not match this login attempt")
	}
	return codeartsCallbackResult{Identifier: identifier, Redirect: redirect}, nil
}

// handleCodeArtsCallback records the provider redirect received on the local
// callback listener.
func handleCodeArtsCallback(w http.ResponseWriter, r *http.Request, cbChan chan<- codeartsCallbackResult) {
	identifier := r.URL.Query().Get("identifier")
	redirect := r.URL.Query().Get("redirect")
	select {
	case cbChan <- codeartsCallbackResult{Identifier: identifier, Redirect: redirect}:
	default:
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>CodeArts Login</title></head>` +
		`<body style="display:flex;justify-content:center;align-items:center;height:100vh;` +
		`font-family:system-ui;background:#1a1a2e;color:#e0e0e0">` +
		`<div style="text-align:center">` +
		`<h1 style="color:#4CAF50">&#10003; Login Successful</h1>` +
		`<p>You can close this window and return to the terminal.</p>` +
		`</div></body></html>`))
}

func (a CodeArtsAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("codearts: failed to find free port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	ticketID := generateCodeArtsTicketID()
	cbChan := make(chan codeartsCallbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		handleCodeArtsCallbackForTicket(w, r, ticketID, cbChan)
	})

	srv := &http.Server{Handler: mux}
	go func() {
		if errServe := srv.Serve(listener); errServe != nil && !strings.Contains(errServe.Error(), "Server closed") {
			log.Warnf("codearts callback server error: %v", errServe)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	codeartsAuth := codearts.NewCodeArtsAuth(nil)
	authURL := codeartsAuth.AuthorizationURL(ticketID, port)

	challengeDelivered := NotifyLoginChallenge(opts, LoginChallenge{URL: authURL})
	if !challengeDelivered {
		if !opts.NoBrowser {
			fmt.Println("Opening browser for CodeArts authentication")
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
	if opts.Prompt != nil {
		go func() {
			pasted, errPrompt := opts.Prompt("Open the CodeArts authentication page in your browser, then paste the full callback URL (it contains identifier and redirect)")
			if errPrompt != nil {
				return
			}
			result, errParse := parseCodeArtsCallbackURL(pasted, ticketID)
			if errParse != nil {
				select {
				case cbChan <- codeartsCallbackResult{Error: errParse.Error()}:
				default:
				}
				return
			}
			select {
			case cbChan <- result:
			default:
			}
		}()
	}

	fmt.Println("Waiting for CodeArts authentication callback...")

	var cbRes codeartsCallbackResult
	timeoutTimer := time.NewTimer(5 * time.Minute)
	defer timeoutTimer.Stop()

	select {
	case cbRes = <-cbChan:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeoutTimer.C:
		return nil, fmt.Errorf("codearts: authentication timed out")
	}

	if cbRes.Error != "" {
		return nil, fmt.Errorf("codearts: authentication failed: %s", cbRes.Error)
	}
	if cbRes.Identifier == "" {
		return nil, fmt.Errorf("codearts: missing identifier in callback")
	}

	fmt.Println("Callback received, polling for login result...")

	pollCtx, pollCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer pollCancel()

	authResult, err := codeartsAuth.PollForLoginResult(pollCtx, ticketID, cbRes.Identifier)
	if err != nil {
		return nil, fmt.Errorf("codearts: %w", err)
	}

	tokenData, err := codeartsAuth.ProcessLoginResult(ctx, authResult)
	if err != nil {
		return nil, fmt.Errorf("codearts: %w", err)
	}

	label := tokenData.UserName
	if label == "" {
		label = "codearts"
	}

	fmt.Println("CodeArts authentication successful")

	return &coreauth.Auth{
		ID:       fmt.Sprintf("codearts-%s.json", sanitizeProviderFileNamePart(tokenData.UserName)),
		Provider: "codearts",
		FileName: fmt.Sprintf("codearts-%s.json", sanitizeProviderFileNamePart(tokenData.UserName)),
		Label:    label,
		Metadata: map[string]any{
			"type":           "codearts",
			"ak":             tokenData.AK,
			"sk":             tokenData.SK,
			"security_token": tokenData.SecurityToken,
			"x_auth_token":   tokenData.XAuthToken,
			"expires_at":     tokenData.ExpiresAt.Format(time.RFC3339),
			"user_id":        tokenData.UserID,
			"user_name":      tokenData.UserName,
			"domain_id":      tokenData.DomainID,
			"email":          tokenData.Email,
		},
	}, nil
}

func generateCodeArtsTicketID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// sanitizeProviderFileNamePart keeps provider-supplied identifiers usable as a
// single filename component: path separators and unsafe characters are
// replaced, leading/trailing dots and dashes are trimmed, and the value is
// capped so generated credential paths stay bounded.
func sanitizeProviderFileNamePart(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.', r == '@', r == '+':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return "unknown"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
