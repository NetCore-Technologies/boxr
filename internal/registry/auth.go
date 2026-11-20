package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxAuthFileBytes = 1 << 20

type authFile struct {
	Auths       map[string]authEntry `json:"auths"`
	CredsStore  string               `json:"credsStore"`
	CredHelpers map[string]string    `json:"credHelpers"`
}

type authEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

// ResolveCredentials prefers explicitly supplied environment credentials,
// then reads an explicit auth file, REGISTRY_AUTH_FILE, Docker config.json, or
// Podman auth.json. Docker credential-helper executables are intentionally not
// invoked: running an external credential helper implicitly would execute
// arbitrary user-configured programs during an image operation.
func ResolveCredentials(reference, authFile string, fallback Credentials) (Credentials, error) {
	parsed, err := ParseReference(reference)
	if err != nil {
		return Credentials{}, err
	}
	if fallback.Username != "" || fallback.Password != "" {
		return fallback, nil
	}
	path, explicit, err := discoverAuthFile(authFile)
	if err != nil {
		return Credentials{}, err
	}
	if path == "" {
		return Credentials{}, nil
	}
	credentials, found, helper, err := credentialsFromFile(path, parsed.Registry)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return Credentials{}, nil
	}
	if err != nil {
		return Credentials{}, err
	}
	if found {
		return credentials, nil
	}
	if helper != "" {
		return Credentials{}, fmt.Errorf("registry %q uses Docker credential helper %q; Boxr does not execute credential helpers; provide BOXR_REGISTRY_USERNAME/BOXR_REGISTRY_PASSWORD or an auth file with inline credentials", parsed.Registry, helper)
	}
	return Credentials{}, nil
}

func discoverAuthFile(explicit string) (string, bool, error) {
	if explicit != "" {
		return explicit, true, nil
	}
	if path := os.Getenv("REGISTRY_AUTH_FILE"); path != "" {
		return path, true, nil
	}
	if dockerConfig := os.Getenv("DOCKER_CONFIG"); dockerConfig != "" {
		return filepath.Join(dockerConfig, "config.json"), false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, nil
	}
	dockerPath := filepath.Join(home, ".docker", "config.json")
	if _, err := os.Stat(dockerPath); err == nil {
		return dockerPath, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("inspect Docker auth file: %w", err)
	}
	podmanPath := filepath.Join(home, ".config", "containers", "auth.json")
	if _, err := os.Stat(podmanPath); err == nil {
		return podmanPath, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("inspect Podman auth file: %w", err)
	}
	return "", false, nil
}

func credentialsFromFile(path, registryHost string) (Credentials, bool, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Credentials{}, false, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Credentials{}, false, "", fmt.Errorf("registry auth file must be a regular non-symlink file")
	}
	if info.Size() > maxAuthFileBytes {
		return Credentials{}, false, "", fmt.Errorf("registry auth file exceeds %d bytes", maxAuthFileBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return Credentials{}, false, "", err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return Credentials{}, false, "", err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return Credentials{}, false, "", fmt.Errorf("registry auth file changed while opening; refusing unsafe file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAuthFileBytes+1))
	if err != nil {
		return Credentials{}, false, "", err
	}
	if len(data) > maxAuthFileBytes {
		return Credentials{}, false, "", fmt.Errorf("registry auth file exceeds %d bytes", maxAuthFileBytes)
	}
	var config authFile
	if err := json.Unmarshal(data, &config); err != nil {
		return Credentials{}, false, "", fmt.Errorf("decode registry auth file: %w", err)
	}
	for key, entry := range config.Auths {
		if !sameRegistryHost(key, registryHost) {
			continue
		}
		if entry.Username != "" || entry.Password != "" {
			if entry.Username == "" {
				return Credentials{}, false, "", fmt.Errorf("registry auth entry for %q has a password but no username", registryHost)
			}
			return Credentials{Username: entry.Username, Password: entry.Password}, true, "", nil
		}
		if entry.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				return Credentials{}, false, "", fmt.Errorf("decode inline registry auth for %q: invalid base64", registryHost)
			}
			username, password, ok := strings.Cut(string(decoded), ":")
			if !ok || username == "" {
				return Credentials{}, false, "", fmt.Errorf("decode inline registry auth for %q: expected base64(username:password)", registryHost)
			}
			return Credentials{Username: username, Password: password}, true, "", nil
		}
		if entry.IdentityToken != "" {
			return Credentials{Username: "oauth2accesstoken", Password: entry.IdentityToken}, true, "", nil
		}
		if entry.RegistryToken != "" {
			return Credentials{Username: "oauth2accesstoken", Password: entry.RegistryToken}, true, "", nil
		}
	}
	for key, helper := range config.CredHelpers {
		if sameRegistryHost(key, registryHost) && helper != "" {
			return Credentials{}, false, helper, nil
		}
	}
	if config.CredsStore != "" {
		return Credentials{}, false, config.CredsStore, nil
	}
	return Credentials{}, false, "", nil
}

func sameRegistryHost(configured, requested string) bool {
	configured = strings.TrimSpace(configured)
	if strings.Contains(configured, "://") {
		configured = strings.TrimPrefix(configured, "https://")
		configured = strings.TrimPrefix(configured, "http://")
	}
	configured = strings.TrimSuffix(configured, "/")
	if index := strings.IndexByte(configured, '/'); index >= 0 {
		configured = configured[:index]
	}
	canonical := func(host string) string {
		host = strings.ToLower(host)
		if host == "docker.io" || host == "index.docker.io" || host == "https://index.docker.io/v1" {
			return "registry-1.docker.io"
		}
		return host
	}
	return canonical(configured) == canonical(requested)
}
