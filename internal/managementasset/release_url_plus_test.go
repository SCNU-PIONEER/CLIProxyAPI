package managementasset

import "testing"

func TestPlusResolveReleaseURL(t *testing.T) {
	for _, tt := range []struct{ name, repo, want string }{
		{"empty", "", defaultManagementReleaseURL},
		{"repository", "https://github.com/example/panel", "https://api.github.com/repos/example/panel/releases/latest"},
		{"git suffix", "https://github.com/example/panel.git", "https://api.github.com/repos/example/panel/releases/latest"},
		{"api repository", "https://api.github.com/repos/example/panel", "https://api.github.com/repos/example/panel/releases/latest"},
		{"api releases", "https://api.github.com/repos/example/panel/releases", "https://api.github.com/repos/example/panel/releases/latest"},
		{"latest", "https://api.github.com/repos/example/panel/releases/latest", "https://api.github.com/repos/example/panel/releases/latest"},
		{"release ID", "https://api.github.com/repos/example/panel/releases/313351637", "https://api.github.com/repos/example/panel/releases/313351637"},
		{"release tag", "https://api.github.com/repos/example/panel/releases/tags/v1.8.0", "https://api.github.com/repos/example/panel/releases/tags/v1.8.0"},
		{"incomplete tag", "https://api.github.com/repos/example/panel/releases/tags", "https://api.github.com/repos/example/panel/releases/latest"},
		{"invalid", "not a url", defaultManagementReleaseURL},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveReleaseURL(tt.repo); got != tt.want {
				t.Fatalf("resolveReleaseURL(%q) = %q, want %q", tt.repo, got, tt.want)
			}
		})
	}
}
