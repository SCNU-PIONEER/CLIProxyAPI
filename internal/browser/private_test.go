package browser

import (
	"errors"
	"reflect"
	"testing"
)

func TestPrivateBrowserCommandKeepsURLAsSingleArgument(t *testing.T) {
	url := "https://example.test/login?state=secret&code=one;two"
	for _, tc := range []struct{ os, executable, flag string }{
		{"linux", "firefox", "--private-window"},
		{"windows", "msedge.exe", "--inprivate"},
		{"darwin", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "--incognito"},
	} {
		t.Run(tc.os, func(t *testing.T) {
			cmd, err := privateBrowserCommand(tc.os, url, func(name string) (string, error) {
				if name == tc.executable {
					return name, nil
				}
				return "", errors.New("not installed")
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cmd.Args, []string{tc.executable, tc.flag, url}) {
				t.Fatalf("unexpected command: %v", cmd.Args)
			}
		})
	}
}

func TestPrivateBrowserUnavailableDoesNotFallBackToNormalWindow(t *testing.T) {
	cmd, err := privateBrowserCommand("linux", "https://example.test/", func(string) (string, error) {
		return "", errors.New("not installed")
	})
	if err == nil || cmd != nil {
		t.Fatal("private mode must fail closed when no supported browser exists")
	}
}
