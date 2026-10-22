package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	btauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/bt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

type BTExecutor struct {
	cfg *config.Config
}

func NewBTExecutor(cfg *config.Config) *BTExecutor {
	return &BTExecutor{cfg: cfg}
}

func (e *BTExecutor) Identifier() string { return "bt" }

func btCredentials(auth *cliproxyauth.Auth) (uid, accessKey, serverID string) {
	uid = metaStringValue(auth.Metadata, "uid")
	accessKey = metaStringValue(auth.Metadata, "access_key")
	serverID = metaStringValue(auth.Metadata, "serverid")
	return
}

func (e *BTExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	uid, accessKey, serverID := btCredentials(auth)
	if uid == "" || accessKey == "" {
		return fmt.Errorf("bt executor: missing credentials")
	}
	req.Header.Set("uid", uid)
	req.Header.Set("access-key", accessKey)
	req.Header.Set("serverid", serverID)
	req.Header.Set("appid", btauth.AppID)
	req.Header.Set("Content-Type", "application/json")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

func (e *BTExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("bt executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *BTExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	endpoint := "/chat/completions"
	if opts.Alt == "responses/compact" {
		to = sdktranslator.FromString("openai-response")
		endpoint = "/responses/compact"
	}
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, opts.Stream)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, opts.Stream)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)

	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}
	if opts.Alt == "responses/compact" {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream"); errDelete == nil {
			translated = updated
		}
	}
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated, requestedModel, requestPath, opts.Headers)

	uid, accessKey, serverID := btCredentials(auth)
	if uid == "" || accessKey == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "bt: missing credentials in auth metadata"}
		return resp, err
	}

	baseURL := btauth.CloudURL + "/plugin_api/chat/openai/v1"
	upstreamURL := strings.TrimSuffix(baseURL, "/") + endpoint

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(translated))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("uid", uid)
	httpReq.Header.Set("access-key", accessKey)
	httpReq.Header.Set("serverid", serverID)
	httpReq.Header.Set("appid", btauth.AppID)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	httpReq.Header.Set("User-Agent", "cli-proxy-bt")
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       upstreamURL,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      translated,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("bt executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("bt executor: request error, status: %d, message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b)}
		return resp, err
	}
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(body))
	reporter.EnsurePublished(ctx)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, body, &param)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *BTExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)

	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	translated, _ = sjson.SetBytes(translated, "stream_options.include_usage", true)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated, requestedModel, requestPath, opts.Headers)

	uid, accessKey, serverID := btCredentials(auth)
	if uid == "" || accessKey == "" {
		return nil, fmt.Errorf("bt: missing credentials in auth metadata")
	}

	baseURL := btauth.CloudURL + "/plugin_api/chat/openai/v1"
	upstreamURL := strings.TrimSuffix(baseURL, "/") + "/chat/completions"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("uid", uid)
	httpReq.Header.Set("access-key", accessKey)
	httpReq.Header.Set("serverid", serverID)
	httpReq.Header.Set("appid", btauth.AppID)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	httpReq.Header.Set("User-Agent", "cli-proxy-bt")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       upstreamURL,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      translated,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("bt executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b)}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("bt executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
			if len(line) == 0 {
				continue
			}
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		} else {
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, []byte("data: [DONE]"), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
			}
		}
		reporter.EnsurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *BTExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := helps.TokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("bt executor: tokenizer init failed: %w", err)
	}

	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("bt executor: token counting failed: %w", err)
	}

	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

const btRefreshTimeout = 15 * time.Second

// btRefreshLogin is a seam so refresh tests never touch a live BaoTa account.
var btRefreshLogin = btauth.LoginContext

// btConfigEntryForAuth resolves the config entry that owns a runtime BT auth.
// The runtime auth intentionally carries no password, so the entry is matched
// on the exact presentation identity written by the config activator: phone,
// proxy URL and model prefix. A missing or ambiguous match is an error: it is
// never safe to refresh a different account than the one the config describes.
func btConfigEntryForAuth(cfg *config.Config, auth *cliproxyauth.Auth) (config.BTKey, error) {
	if cfg == nil {
		return config.BTKey{}, fmt.Errorf("bt: configuration is not available")
	}
	if auth == nil {
		return config.BTKey{}, fmt.Errorf("bt: auth is nil")
	}
	phone := ""
	if auth.Attributes != nil {
		phone = strings.TrimSpace(auth.Attributes["bt_phone"])
	}
	if phone == "" && auth.Metadata != nil {
		phone = strings.TrimSpace(metaStringValue(auth.Metadata, "bt_phone"))
	}
	if phone == "" {
		return config.BTKey{}, fmt.Errorf("bt: auth %s is missing its config phone", auth.ID)
	}
	proxyURL := strings.TrimSpace(auth.ProxyURL)
	prefix := strings.TrimSpace(auth.Prefix)
	matches := make([]config.BTKey, 0, 1)
	for i := range cfg.BTKey {
		entry := cfg.BTKey[i]
		if !strings.EqualFold(strings.TrimSpace(entry.Phone), phone) {
			continue
		}
		if strings.TrimSpace(entry.ProxyURL) != proxyURL {
			continue
		}
		if strings.TrimSpace(entry.Prefix) != prefix {
			continue
		}
		matches = append(matches, entry)
	}
	switch len(matches) {
	case 0:
		return config.BTKey{}, fmt.Errorf("bt: no config entry matches phone %s (proxy_set=%t, prefix %q); refusing to refresh", btauth.MaskPhone(phone), proxyURL != "", prefix)
	case 1:
		return matches[0], nil
	default:
		return config.BTKey{}, fmt.Errorf("bt: %d config entries match phone %s (proxy_set=%t, prefix %q); refusing ambiguous refresh", len(matches), btauth.MaskPhone(phone), proxyURL != "", prefix)
	}
}

// Refresh re-acquires a config-managed BT credential. The password is read from
// the in-memory config entry and is never written back into the auth record.
func (e *BTExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("bt executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("bt: auth is nil")
	}
	entry, err := btConfigEntryForAuth(e.cfg, auth)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	loginCtx, cancel := context.WithTimeout(ctx, btRefreshTimeout)
	defer cancel()
	token, err := btRefreshLogin(loginCtx, entry.Phone, entry.Password, entry.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("bt: refresh login failed: %w", err)
	}
	if token == nil || strings.TrimSpace(token.UID) == "" {
		return nil, fmt.Errorf("bt: refresh login returned no uid")
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["type"] = "bt"
	updated.Metadata["uid"] = token.UID
	updated.Metadata["access_key"] = token.AccessKey
	updated.Metadata["serverid"] = token.ServerID
	return updated, nil
}

func FetchBTModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	if auth == nil {
		return nil
	}

	uid, accessKey, serverID := btCredentials(auth)
	if uid == "" || accessKey == "" {
		log.Debug("bt: missing credentials, skipping dynamic model fetch")
		return nil
	}

	httpClient := newProxyAwareHTTPClient(ctx, cfg, auth, 15*time.Second)
	modelsURL := btauth.CloudURL + "/plugin_api/chat/openai/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		log.Warnf("bt: failed to create model fetch request: %v", err)
		return nil
	}
	req.Header.Set("uid", uid)
	req.Header.Set("access-key", accessKey)
	req.Header.Set("serverid", serverID)
	req.Header.Set("appid", btauth.AppID)
	req.Header.Set("User-Agent", "cli-proxy-bt")
	util.ApplyCustomHeadersFromAttrs(req, auth.Attributes)

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Warnf("bt: fetch models failed: %v", err)
		return nil
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Debugf("bt: close model fetch response error: %v", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Warnf("bt: failed to read models response: %v", err)
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		log.Warnf("bt: fetch models failed: status %d, body: %s", resp.StatusCode, helps.SummarizeErrorBody(resp.Header.Get("Content-Type"), body))
		return nil
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		log.Warnf("bt: failed to parse models response: %v", err)
		return nil
	}

	now := time.Now().Unix()
	models := make([]*registry.ModelInfo, 0, len(result.Data))
	seen := make(map[string]struct{})
	for _, m := range result.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, &registry.ModelInfo{
			ID:          id,
			Object:      "model",
			Created:     now,
			OwnedBy:     "bt",
			Type:        "bt",
			DisplayName: id,
			UserDefined: false,
			Thinking:    &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}},
		})
	}
	return models
}
