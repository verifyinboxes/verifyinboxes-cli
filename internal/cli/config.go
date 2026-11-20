package cli

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/verifyinboxes/verifyinboxes-go"
)

// Environment variables the tool reads.
const (
	envAPIKey    = verifyinboxes.EnvAPIKey
	envBaseURL   = verifyinboxes.EnvBaseURL
	envConfigDir = "VERIFYINBOXES_CONFIG_DIR"
)

// baseURL is the API's address: --base-url, else VERIFYINBOXES_BASE_URL, else the SDK's default, without a
// trailing slash. It names what is saved, too.
func (a *App) baseURL() (string, error) {
	value := strings.TrimSpace(a.baseURLFlag)
	if value == "" {
		value = strings.TrimSpace(os.Getenv(envBaseURL))
	}
	if value == "" {
		value = verifyinboxes.DefaultBaseURL
	}
	value = strings.TrimSuffix(value, "/")
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", usageError(fmt.Sprintf("the base URL %q is not an http:// or https:// address", value),
			"Check --base-url and "+envBaseURL+".")
	}
	return value, nil
}

// envKey is VERIFYINBOXES_API_KEY, trimmed.
func envKey() string { return strings.TrimSpace(os.Getenv(envAPIKey)) }

// credentialsFile is where what is saved is kept when it is not in the keychain: credentials.json in
// VERIFYINBOXES_CONFIG_DIR (chosen: the keychain is then not used at all), else in verifyinboxes/ under the user's
// configuration directory (~/.config on Linux, ~/Library/Application Support on macOS, %AppData% on Windows); "" when
// there is none.
func credentialsFile() (path string, chosen bool) {
	if dir := strings.TrimSpace(os.Getenv(envConfigDir)); dir != "" {
		return filepath.Join(dir, "credentials.json"), true
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "verifyinboxes", "credentials.json"), false
	}
	return "", false
}
