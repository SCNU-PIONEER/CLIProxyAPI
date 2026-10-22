package openai

import (
	. "github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/translator"
)

func init() {
	translator.Register(
		OpenAI,
		CodeArts,
		convertOpenAIRequestToCodeArts,
		interfaces.TranslateResponse{
			Stream:    ConvertCodeArtsStreamToOpenAI,
			NonStream: ConvertCodeArtsNonStreamToOpenAI,
		},
	)
}

// convertOpenAIRequestToCodeArts adapts the legacy converter to the v8
// transform signature. The converter itself cannot fail; the error is part of
// the registration contract and is always nil.
func convertOpenAIRequestToCodeArts(model string, rawJSON []byte, stream bool) ([]byte, error) {
	return ConvertOpenAIRequestToCodeArts(model, rawJSON, stream), nil
}
