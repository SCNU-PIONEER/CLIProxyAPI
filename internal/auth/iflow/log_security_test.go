package iflow

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

type secretResponseTransport struct{ status int }

func (tr secretResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: tr.status, Body: io.NopCloser(strings.NewReader(`{"refresh_token":"SECRET-MARKER-IFLOW"}`)), Header: make(http.Header)}, nil
}

func TestTokenErrorsDoNotExposeResponseSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := log.StandardLogger()
	previousOutput, previousLevel := logger.Out, logger.GetLevel()
	logger.SetOutput(&output)
	logger.SetLevel(log.DebugLevel)
	t.Cleanup(func() { logger.SetOutput(previousOutput); logger.SetLevel(previousLevel) })
	for _, status := range []int{http.StatusUnauthorized, http.StatusOK} {
		output.Reset()
		client := &IFlowAuth{httpClient: &http.Client{Transport: secretResponseTransport{status: status}}}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://iflow.invalid/token", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.doTokenRequest(t.Context(), req)
		if err == nil {
			t.Fatalf("status %d unexpectedly succeeded", status)
		}
		if strings.Contains(err.Error()+output.String(), "SECRET-MARKER-IFLOW") {
			t.Fatalf("status %d exposed a response secret in an error or log", status)
		}
	}
}
