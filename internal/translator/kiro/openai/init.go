// Package openai provides translation between OpenAI Chat Completions and Kiro formats.
package openai

import (
	. "github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/translator"
)

func init() {
	translator.Register(
		OpenAI, // source format
		Kiro,   // target format
		convertOpenAIRequestToKiro,
		interfaces.TranslateResponse{
			Stream:    ConvertKiroStreamToOpenAI,
			NonStream: ConvertKiroNonStreamToOpenAI,
		},
	)
}

// convertOpenAIRequestToKiro adapts the legacy converter to the v8 transform
// signature. The converter itself cannot fail; the error is part of the
// registration contract and is always nil.
func convertOpenAIRequestToKiro(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return ConvertOpenAIRequestToKiro(modelName, inputRawJSON, stream), nil
}
