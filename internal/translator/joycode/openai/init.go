package openai

import (
	. "github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/translator"
)

func init() {
	translator.Register(
		OpenAI,
		JoyCode,
		convertOpenAIRequestToJoyCode,
		interfaces.TranslateResponse{
			Stream:    ConvertJoyCodeStreamToOpenAI,
			NonStream: ConvertJoyCodeNonStreamToOpenAI,
		},
	)
}

// convertOpenAIRequestToJoyCode adapts the legacy converter to the v8 transform
// signature. The converter itself cannot fail; the error is part of the
// registration contract and is always nil.
func convertOpenAIRequestToJoyCode(model string, rawJSON []byte, stream bool) ([]byte, error) {
	return ConvertOpenAIRequestToJoyCode(model, rawJSON, stream), nil
}
