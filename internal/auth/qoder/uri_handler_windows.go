//go:build windows

package qoder

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

// ErrURIHandlerOptInRequired is returned when installing the qoder:// handler
// was not explicitly requested. Installing an OS-level protocol handler is a
// persistent machine change, so callers must pass allowInstall=true.
var ErrURIHandlerOptInRequired = errors.New("qoder: URI handler installation requires explicit opt-in")

// RegisterURIHandler registers the qoder:// URI protocol handler on Windows.
// It creates a VBS script that forwards the qoder:// callback URL to the local
// HTTP callback server, then registers the protocol in the Windows registry.
// The script is written to a randomly named file so a predictable path cannot
// be pre-created by another local process. Installation is skipped unless
// allowInstall is true; without it the caller should use the manual callback
// URL path instead.
// Returns a cleanup function that should be deferred.
func RegisterURIHandler(callbackPort int, callbackState string, allowInstall bool) (func(), error) {
	if !allowInstall {
		return nil, ErrURIHandlerOptInRequired
	}

	script, err := os.CreateTemp("", "qoder_login_handler_*.vbs")
	if err != nil {
		return nil, fmt.Errorf("qoder: create VBS handler script: %w", err)
	}
	vbsPath := script.Name()

	vbsContent := fmt.Sprintf(`Set objHTTP = CreateObject("MSXML2.XMLHTTP")
On Error Resume Next
url = "http://127.0.0.1:%d/forward/%s?url="
url = url & UrlEncode(WScript.Arguments(0))
objHTTP.Open "GET", url, False
objHTTP.send

Function UrlEncode(str)
  Dim result, i, c
  result = ""
  For i = 1 To Len(str)
    c = Mid(str, i, 1)
    If c = " " Then
      result = result & "+"
    ElseIf InStr("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.~", c) > 0 Then
      result = result & c
    Else
      result = result & "%%" & Right("0" & Hex(Asc(c)), 2)
    End If
  Next
  UrlEncode = result
End Function
`, callbackPort, callbackState)

	if _, err = script.WriteString(vbsContent); err != nil {
		_ = script.Close()
		_ = os.Remove(vbsPath)
		return nil, fmt.Errorf("qoder: write VBS handler script: %w", err)
	}
	if err = script.Close(); err != nil {
		_ = os.Remove(vbsPath)
		return nil, fmt.Errorf("qoder: close VBS handler script: %w", err)
	}

	regCmds := [][]string{
		{"reg", "add", `HKCU\Software\Classes\qoder`, "/ve", "/t", "REG_SZ", "/d", "URL:QoderLogin", "/f"},
		{"reg", "add", `HKCU\Software\Classes\qoder`, "/v", "URL Protocol", "/t", "REG_SZ", "/d", "", "/f"},
		{"reg", "add", `HKCU\Software\Classes\qoder\shell`, "/f"},
		{"reg", "add", `HKCU\Software\Classes\qoder\shell\open`, "/f"},
		{"reg", "add", `HKCU\Software\Classes\qoder\shell\open\command`,
			"/ve", "/t", "REG_SZ", "/d", fmt.Sprintf(`wscript.exe "%s" %%1`, vbsPath), "/f"},
	}

	for _, args := range regCmds {
		cmd := exec.Command(args[0], args[1:]...)
		if err = cmd.Run(); err != nil {
			unregisterURIHandlerAt(vbsPath)
			return nil, fmt.Errorf("qoder: register URI handler command %q: %w", args[1], err)
		}
	}

	log.Infof("qoder: registered qoder:// URI handler (VBS: %s)", filepath.Base(vbsPath))

	return func() {
		unregisterURIHandlerAt(vbsPath)
	}, nil
}

// UnregisterURIHandler removes the qoder:// URI protocol handler from Windows
// registry and cleans up the temporary VBS script previously written by older
// builds at the fixed temporary path.
func UnregisterURIHandler() {
	unregisterURIHandlerAt(filepath.Join(os.TempDir(), "qoder_login_handler.vbs"))
}

func unregisterURIHandlerAt(vbsPath string) {
	cmd := exec.Command("reg", "delete", `HKCU\Software\Classes\qoder`, "/f")
	_ = cmd.Run()

	_ = os.Remove(vbsPath)

	log.Info("qoder: unregistered qoder:// URI handler")
}
