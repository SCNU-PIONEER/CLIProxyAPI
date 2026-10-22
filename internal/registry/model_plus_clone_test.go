package registry

import "testing"

func TestCloneModelInfoIsolatesPlusSupportedEndpoints(t *testing.T) {
	original := &ModelInfo{ID: "plus", SupportedEndpoints: []string{"/chat/completions"}}
	copy := cloneModelInfo(original)
	copy.SupportedEndpoints[0] = "/responses"
	if original.SupportedEndpoints[0] != "/chat/completions" {
		t.Fatal("cloned Plus model endpoints share mutable storage")
	}
}
