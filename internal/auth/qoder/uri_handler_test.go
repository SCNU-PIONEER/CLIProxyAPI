package qoder

import (
	"errors"
	"testing"
)

func TestRegisterURIHandlerRequiresOptIn(t *testing.T) {
	cleanup, err := RegisterURIHandler(8317, "state123", false)
	if !errors.Is(err, ErrURIHandlerOptInRequired) {
		t.Fatalf("RegisterURIHandler error = %v, want ErrURIHandlerOptInRequired", err)
	}
	if cleanup != nil {
		t.Fatal("RegisterURIHandler returned a cleanup func without installing")
	}
}
