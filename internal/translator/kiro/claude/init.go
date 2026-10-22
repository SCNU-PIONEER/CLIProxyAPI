// Package claude provides translation between Kiro and Claude formats.
package claude

import (
	. "github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/translator"
)

func init() {
	translator.Register(
		Claude,
		Kiro,
		convertClaudeRequestToKiro,
		interfaces.TranslateResponse{
			Stream:    ConvertKiroStreamToClaude,
			NonStream: ConvertKiroNonStreamToClaude,
		},
	)
}

// convertClaudeRequestToKiro adapts the legacy converter to the v8 transform
// signature. The converter itself cannot fail; the error is part of the
// registration contract and is always nil.
func convertClaudeRequestToKiro(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return ConvertClaudeRequestToKiro(modelName, inputRawJSON, stream), nil
}
