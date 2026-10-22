package browser

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
)

var incognitoMode atomic.Bool

// SetIncognitoMode selects private windows for subsequent browser launches.
func SetIncognitoMode(enabled bool) { incognitoMode.Store(enabled) }

// IsIncognitoMode reports the process-wide browser preference.
func IsIncognitoMode() bool { return incognitoMode.Load() }

// CloseBrowser is retained for source compatibility. Browser launchers may
// reuse an existing user-owned process, so it is unsafe to kill that process.
// The user closes the private window after finishing authorization.
func CloseBrowser() error { return nil }

func openPrivateURL(url string) error {
	cmd, err := privateBrowserCommand(runtime.GOOS, url, exec.LookPath)
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch private browser: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func privateBrowserCommand(goos, url string, lookup func(string) (string, error)) (*exec.Cmd, error) {
	type candidate struct{ executable, flag string }
	var candidates []candidate
	switch goos {
	case "windows":
		candidates = []candidate{{"msedge.exe", "--inprivate"}, {"chrome.exe", "--incognito"}, {"firefox.exe", "--private-window"}}
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
			if root == "" {
				continue
			}
			candidates = append(candidates,
				candidate{filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"), "--inprivate"},
				candidate{filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"), "--incognito"},
				candidate{filepath.Join(root, "Mozilla Firefox", "firefox.exe"), "--private-window"})
		}
	case "darwin":
		candidates = []candidate{{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "--incognito"}, {"/Applications/Firefox.app/Contents/MacOS/firefox", "--private-window"}, {"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge", "--inprivate"}}
	case "linux":
		candidates = []candidate{{"google-chrome", "--incognito"}, {"chromium", "--incognito"}, {"chromium-browser", "--incognito"}, {"firefox", "--private-window"}}
	}
	for _, candidate := range candidates {
		if executable, err := lookup(candidate.executable); err == nil {
			return exec.Command(executable, candidate.flag, url), nil
		}
	}
	return nil, fmt.Errorf("no supported private browser found for %s; use no-browser mode and open a private window manually", goos)
}
