package executor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tiktoken-go/tokenizer"
)

func newProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	return helps.NewProxyAwareHTTPClient(ctx, cfg, auth, timeout)
}

func parseOpenAIUsage(data []byte) usage.Detail {
	return helps.ParseOpenAIUsage(data)
}

func parseOpenAIStreamUsage(line []byte) (usage.Detail, bool) {
	return helps.ParseOpenAIStreamUsage(line)
}

func parseOpenAIResponsesUsage(data []byte) usage.Detail {
	return helps.ParseOpenAIUsage(data)
}

func parseOpenAIResponsesStreamUsage(line []byte) (usage.Detail, bool) {
	return helps.ParseOpenAIStreamUsage(line)
}

func getTokenizer(model string) (tokenizer.Codec, error) {
	return helps.TokenizerForModel(model)
}

func countOpenAIChatTokens(enc tokenizer.Codec, payload []byte) (int64, error) {
	return helps.CountOpenAIChatTokens(enc, payload)
}

func countClaudeChatTokens(enc tokenizer.Codec, payload []byte) (int64, error) {
	return helps.CountClaudeChatTokens(enc, payload)
}

func buildOpenAIUsageJSON(count int64) []byte {
	return helps.BuildOpenAIUsageJSON(count)
}

type upstreamRequestLog = helps.UpstreamRequestLog

func recordAPIRequest(ctx context.Context, cfg *config.Config, info upstreamRequestLog) {
	helps.RecordAPIRequest(ctx, cfg, info)
}

func recordAPIResponseMetadata(ctx context.Context, cfg *config.Config, status int, headers http.Header) {
	helps.RecordAPIResponseMetadata(ctx, cfg, status, headers)
}

func recordAPIResponseError(ctx context.Context, cfg *config.Config, err error) {
	helps.RecordAPIResponseError(ctx, cfg, err)
}

func appendAPIResponseChunk(ctx context.Context, cfg *config.Config, chunk []byte) {
	helps.AppendAPIResponseChunk(ctx, cfg, chunk)
}

func payloadRequestedModel(opts cliproxyexecutor.Options, fallback string) string {
	return helps.PayloadRequestedModel(opts, fallback)
}

func applyPayloadConfigWithRoot(cfg *config.Config, model, protocol, root string, payload, original []byte, requestedModel string) []byte {
	return helps.ApplyPayloadConfigWithRoot(cfg, model, protocol, root, payload, original, requestedModel, "")
}

func summarizeErrorBody(contentType string, body []byte) string {
	return helps.SummarizeErrorBody(contentType, body)
}

func apiKeyFromContext(ctx context.Context) string {
	return helps.APIKeyFromContext(ctx)
}

func tokenizerForModel(model string) (tokenizer.Codec, error) {
	return helps.TokenizerForModel(model)
}

func collectOpenAIContent(content gjson.Result, segments *[]string) {
	helps.CollectOpenAIContent(content, segments)
}

type usageReporter struct {
	reporter *helps.UsageReporter
}

func newUsageReporter(ctx context.Context, provider, model string, auth *cliproxyauth.Auth) *usageReporter {
	return &usageReporter{reporter: helps.NewUsageReporter(ctx, provider, model, auth)}
}

func (r *usageReporter) publish(ctx context.Context, detail usage.Detail) {
	if r == nil || r.reporter == nil {
		return
	}
	r.reporter.Publish(ctx, detail)
}

func (r *usageReporter) publishFailure(ctx context.Context) {
	if r == nil || r.reporter == nil {
		return
	}
	r.reporter.PublishFailure(ctx)
}

func (r *usageReporter) trackFailure(ctx context.Context, errPtr *error) {
	if r == nil || r.reporter == nil {
		return
	}
	r.reporter.TrackFailure(ctx, errPtr)
}

func (r *usageReporter) ensurePublished(ctx context.Context) {
	if r == nil || r.reporter == nil {
		return
	}
	r.reporter.EnsurePublished(ctx)
}

// countTokensWithThinking estimates input tokens for what the upstream would
// see: the source payload is normalized into OpenAI chat form, canonical
// thinking is applied, and the result is counted locally. It performs no I/O,
// so it is safe for providers without a native token-count endpoint.
func countTokensWithThinking(ctx context.Context, cfg *config.Config, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	body, err := helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), provider)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	enc, err := tokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("%s: tokenizer init failed: %w", provider, err)
	}
	count, err := countOpenAIChatTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	out := sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, buildOpenAIUsageJSON(count))
	return cliproxyexecutor.Response{Payload: out, Headers: make(http.Header)}, nil
}
