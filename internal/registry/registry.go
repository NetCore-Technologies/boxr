// Package registry implements the OCI Distribution pull protocol for Boxr's
// single-rootfs image format. It intentionally rejects arbitrary multi-layer
// Docker images until safe overlay-layer application is implemented.
package registry

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	boxrLayerMediaType = "application/vnd.boxr.rootfs.v1.tar+gzip"
	maxManifestBytes   = 4 << 20
	maxConfigBytes     = 4 << 20
	maxLayerBytes      = int64(8 << 30)
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	repoPattern   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	hostPattern   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]{1,5})?$`)
)

type Credentials struct {
	Username string
	Password string
}

type Reference struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

func validRegistryHost(value string) bool {
	if !hostPattern.MatchString(value) || strings.ContainsAny(value, "/?#@") {
		return false
	}
	host, portText, hasPort := strings.Cut(value, ":")
	if hasPort {
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

func (r Reference) Selector() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

func (r Reference) Canonical() string {
	name := r.Registry + "/" + r.Repository
	if r.Digest != "" {
		return name + "@" + r.Digest
	}
	return name + ":" + r.Tag
}

// LocalReference maps immutable digest pulls to a deterministic local tag.
// The local store is tag-oriented, while the original digest remains available
// in Artifact.ManifestDigest for audit and reproducibility.
func (r Reference) LocalReference() string {
	name := r.Registry + "/" + r.Repository
	if r.Digest != "" {
		return name + ":sha256-" + strings.TrimPrefix(r.Digest, "sha256:")
	}
	return name + ":" + r.Tag
}

type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

type index struct {
	SchemaVersion int `json:"schemaVersion"`
	Manifests     []struct {
		Descriptor
		Platform Platform `json:"platform"`
	} `json:"manifests"`
}

type imageConfig struct {
	OS           string        `json:"os"`
	Architecture string        `json:"architecture"`
	Variant      string        `json:"variant,omitempty"`
	Config       RuntimeConfig `json:"config,omitempty"`
	RootFS       struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

type RuntimeConfig struct {
	Env        []string          `json:"Env,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	User       string            `json:"User,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

type Artifact struct {
	Reference      string
	RuntimeConfig  RuntimeConfig
	OS             string
	Architecture   string
	Variant        string
	ManifestDigest string
	ConfigDigest   string
	LayerDigest    string
	DiffID         string
	LayerPath      string
	LayerBytes     int64
}

type PushBundle struct {
	Manifest  []byte
	Config    []byte
	LayerPath string
}

func (a Artifact) Cleanup() error {
	if a.LayerPath == "" {
		return nil
	}
	return os.Remove(a.LayerPath)
}

func ParseReference(value string) (Reference, error) {
	var result Reference
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\\\x00\r\n ") {
		return result, fmt.Errorf("invalid image reference")
	}
	name, selector, hasDigest := value, "", false
	if before, after, ok := strings.Cut(value, "@"); ok {
		name, selector, hasDigest = before, after, true
		if !digestPattern.MatchString(selector) {
			return result, fmt.Errorf("image digest must be a sha256 digest")
		}
	}
	lastSlash := strings.LastIndex(name, "/")
	colon := strings.LastIndex(name, ":")
	if !hasDigest && colon > lastSlash {
		selector, name = name[colon+1:], name[:colon]
	} else if !hasDigest {
		selector = "latest"
	}
	if !hasDigest && !tagPattern.MatchString(selector) {
		return result, fmt.Errorf("invalid image tag %q", selector)
	}
	parts := strings.Split(name, "/")
	registryHost := "docker.io"
	if len(parts) > 1 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost") {
		registryHost = parts[0]
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return result, fmt.Errorf("image repository is empty")
	}
	repository := strings.Join(parts, "/")
	if registryHost == "docker.io" && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	if !repoPattern.MatchString(repository) {
		return result, fmt.Errorf("invalid image repository %q", repository)
	}
	if registryHost == "docker.io" {
		registryHost = "registry-1.docker.io"
	}
	if !validRegistryHost(registryHost) {
		return result, fmt.Errorf("invalid registry host %q", registryHost)
	}
	if hasDigest {
		result.Digest = selector
	} else {
		result.Tag = selector
	}
	result.Registry, result.Repository = registryHost, repository
	return result, nil
}

// Pull fetches a Boxr-format image from an OCI Distribution-compatible registry.
// platform must be OS/architecture[/variant]; an empty value uses the current Linux host.
// The returned compressed layer is stored in a private temporary file and must
// be removed by calling Artifact.Cleanup after importing it.
func Pull(ctx context.Context, reference, platform string, credentials Credentials) (Artifact, error) {
	var result Artifact
	parsed, err := ParseReference(reference)
	if err != nil {
		return result, err
	}
	if platform == "" {
		platform = "linux/" + runtime.GOARCH
	}
	parts := strings.Split(platform, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "linux" || parts[1] == "" || (len(parts) == 3 && parts[2] == "") {
		return result, fmt.Errorf("platform must be linux/ARCH[/VARIANT]")
	}
	osName, arch := parts[0], parts[1]
	requestedVariant := ""
	if len(parts) == 3 {
		requestedVariant = parts[2]
	}
	if strings.ContainsAny(platform, "\\\x00\r\n") {
		return result, fmt.Errorf("invalid platform")
	}
	baseURL := "https://" + parsed.Registry
	if parsed.Registry == "localhost" || strings.HasPrefix(parsed.Registry, "localhost:") || strings.HasPrefix(parsed.Registry, "127.0.0.1:") {
		baseURL = "http://" + parsed.Registry
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	fetcher := registryClient{http: client, baseURL: baseURL, repository: parsed.Repository, credentials: credentials}
	manifestBytes, mediaType, digest, err := fetcher.manifest(ctx, parsed.Selector(), "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	if err != nil {
		return result, err
	}
	if isIndex(mediaType) || looksLikeIndex(manifestBytes) {
		var idx index
		if err := json.Unmarshal(manifestBytes, &idx); err != nil {
			return result, fmt.Errorf("decode registry manifest index: %w", err)
		}
		if idx.SchemaVersion != 2 {
			return result, fmt.Errorf("unsupported registry manifest index schema %d", idx.SchemaVersion)
		}
		matches := make([]struct {
			manifestBytes []byte
			mediaType     string
			digest        string
			variant       string
		}, 0, 2)
		for _, candidate := range idx.Manifests {
			if candidate.Platform.OS == osName && candidate.Platform.Architecture == arch && (requestedVariant == "" || candidate.Platform.Variant == requestedVariant) {
				matches = append(matches, struct {
					manifestBytes []byte
					mediaType     string
					digest        string
					variant       string
				}{digest: candidate.Digest, variant: candidate.Platform.Variant})
			}
		}
		if requestedVariant == "" && len(matches) > 1 {
			return result, fmt.Errorf("registry image has multiple %s/%s variants; specify --platform linux/%s/VARIANT", osName, arch, arch)
		}
		if len(matches) == 0 {
			return result, fmt.Errorf("registry image has no %s/%s manifest", osName, arch)
		}
		selectedVariant := matches[0].variant
		for _, candidate := range idx.Manifests {
			if candidate.Platform.OS == osName && candidate.Platform.Architecture == arch && candidate.Platform.Variant == selectedVariant {
				if candidate.MediaType != "application/vnd.oci.image.manifest.v1+json" && candidate.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
					return result, fmt.Errorf("unsupported platform manifest media type %q", candidate.MediaType)
				}
				if err := validateDescriptor(candidate.Descriptor, maxManifestBytes); err != nil {
					return result, fmt.Errorf("invalid platform manifest descriptor: %w", err)
				}
				manifestBytes, mediaType, digest, err = fetcher.manifest(ctx, candidate.Digest, "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
				if err != nil {
					return result, err
				}
				break
			}
		}
		requestedVariant = selectedVariant
	}
	if mediaType != "application/vnd.oci.image.manifest.v1+json" && mediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		return result, fmt.Errorf("unsupported registry manifest media type %q", mediaType)
	}
	var remote manifest
	if err := json.Unmarshal(manifestBytes, &remote); err != nil {
		return result, fmt.Errorf("decode registry manifest: %w", err)
	}
	if remote.SchemaVersion != 2 || (remote.MediaType != "" && remote.MediaType != mediaType) || len(remote.Layers) != 1 {
		return result, fmt.Errorf("unsupported image manifest: Boxr currently requires a schema-2 manifest with exactly one rootfs layer")
	}
	if remote.Config.MediaType != "application/vnd.oci.image.config.v1+json" && remote.Config.MediaType != "application/vnd.docker.container.image.v1+json" {
		return result, fmt.Errorf("unsupported image config media type %q", remote.Config.MediaType)
	}
	if remote.Layers[0].MediaType != boxrLayerMediaType {
		return result, fmt.Errorf("unsupported layer media type %q; this registry pull currently supports Boxr rootfs layers only", remote.Layers[0].MediaType)
	}
	if err := validateDescriptor(remote.Config, maxConfigBytes); err != nil {
		return result, fmt.Errorf("invalid image config descriptor: %w", err)
	}
	if err := validateDescriptor(remote.Layers[0], maxLayerBytes); err != nil {
		return result, fmt.Errorf("invalid rootfs layer descriptor: %w", err)
	}
	configBytes, _, _, err := fetcher.blob(ctx, remote.Config, maxConfigBytes)
	if err != nil {
		return result, fmt.Errorf("download image config: %w", err)
	}
	var config imageConfig
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return result, fmt.Errorf("decode image config: %w", err)
	}
	if config.OS != osName || config.Architecture != arch || (requestedVariant != "" && config.Variant != "" && config.Variant != requestedVariant) {
		return result, fmt.Errorf("image config platform %s/%s/%s does not match requested %s/%s/%s", config.OS, config.Architecture, config.Variant, osName, arch, requestedVariant)
	}
	if config.Variant == "" {
		config.Variant = requestedVariant
	}
	if config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != 1 || !digestPattern.MatchString(config.RootFS.DiffIDs[0]) {
		return result, fmt.Errorf("unsupported image config rootfs descriptor")
	}
	layerPath, _, _, err := fetcher.blobToTemp(ctx, remote.Layers[0], maxLayerBytes)
	if err != nil {
		return result, fmt.Errorf("download rootfs layer: %w", err)
	}
	diffID, err := uncompressedDigest(layerPath)
	if err != nil {
		_ = os.Remove(layerPath)
		return result, fmt.Errorf("verify uncompressed rootfs layer: %w", err)
	}
	if diffID != config.RootFS.DiffIDs[0] {
		_ = os.Remove(layerPath)
		return result, fmt.Errorf("rootfs layer diff_id mismatch: expected %s, received %s", config.RootFS.DiffIDs[0], diffID)
	}
	return Artifact{Reference: parsed.LocalReference(), RuntimeConfig: config.Config, OS: config.OS, Architecture: config.Architecture, Variant: config.Variant, ManifestDigest: digest, ConfigDigest: remote.Config.Digest, LayerDigest: remote.Layers[0].Digest, DiffID: diffID, LayerPath: layerPath, LayerBytes: remote.Layers[0].Size}, nil
}

type registryClient struct {
	http                *http.Client
	baseURL, repository string
	credentials         Credentials
	token               string
	scope               string
}

func (c *registryClient) manifest(ctx context.Context, selector, accept string) ([]byte, string, string, error) {
	endpoint := c.baseURL + "/v2/" + c.repository + "/manifests/" + url.PathEscape(selector)
	data, contentType, digest, err := c.get(ctx, endpoint, accept, maxManifestBytes)
	if err != nil {
		return nil, "", "", err
	}
	actual := digestBytes(data)
	if digest != "" && digest != actual {
		return nil, "", "", fmt.Errorf("manifest digest header mismatch: expected %s, received %s", digest, actual)
	}
	if strings.HasPrefix(selector, "sha256:") && selector != actual {
		return nil, "", "", fmt.Errorf("requested manifest digest mismatch: expected %s, received %s", selector, actual)
	}
	return data, normalizeMediaType(contentType), actual, nil
}

func (c *registryClient) blob(ctx context.Context, descriptor Descriptor, limit int64) ([]byte, string, string, error) {
	endpoint := c.baseURL + "/v2/" + c.repository + "/blobs/" + url.PathEscape(descriptor.Digest)
	data, media, _, err := c.get(ctx, endpoint, "application/octet-stream", limit)
	if err != nil {
		return nil, "", "", err
	}
	if int64(len(data)) != descriptor.Size {
		return nil, "", "", fmt.Errorf("blob size mismatch: expected %d, received %d", descriptor.Size, len(data))
	}
	actual := digestBytes(data)
	if actual != descriptor.Digest {
		return nil, "", "", fmt.Errorf("blob digest mismatch: expected %s, received %s", descriptor.Digest, actual)
	}
	return data, media, actual, nil
}

func (c *registryClient) blobToTemp(ctx context.Context, descriptor Descriptor, limit int64) (string, string, int64, error) {
	endpoint := c.baseURL + "/v2/" + c.repository + "/blobs/" + url.PathEscape(descriptor.Digest)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", 0, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	response, err := c.do(request)
	if err != nil {
		return "", "", 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", "", 0, responseError(response)
	}
	if response.ContentLength >= 0 && response.ContentLength != descriptor.Size {
		return "", "", 0, fmt.Errorf("layer Content-Length mismatch: expected %d, received %d", descriptor.Size, response.ContentLength)
	}
	file, err := os.CreateTemp("", "boxr-registry-layer-*.tar.gz")
	if err != nil {
		return "", "", 0, err
	}
	path := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, limit+1))
	closeErr := file.Close()
	if copyErr != nil {
		return "", "", 0, copyErr
	}
	if closeErr != nil {
		return "", "", 0, closeErr
	}
	if written > limit {
		return "", "", 0, fmt.Errorf("registry layer exceeds %d bytes", limit)
	}
	if written != descriptor.Size {
		return "", "", 0, fmt.Errorf("layer size mismatch: expected %d, received %d", descriptor.Size, written)
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actual != descriptor.Digest {
		return "", "", 0, fmt.Errorf("layer digest mismatch: expected %s, received %s", descriptor.Digest, actual)
	}
	cleanup = false
	return path, actual, written, nil
}

func (c *registryClient) get(ctx context.Context, endpoint, accept string, limit int64) ([]byte, string, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", "", err
	}
	request.Header.Set("Accept", accept)
	response, err := c.do(request)
	if err != nil {
		return nil, "", "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", "", responseError(response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, "", "", err
	}
	if int64(len(data)) > limit {
		return nil, "", "", fmt.Errorf("registry response exceeds %d bytes", limit)
	}
	return data, response.Header.Get("Content-Type"), response.Header.Get("Docker-Content-Digest"), nil
}

func (c *registryClient) do(request *http.Request) (*http.Response, error) {
	if c.token != "" && request.Header.Get("Authorization") == "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusUnauthorized {
		return response, nil
	}
	challenge := response.Header.Get("WWW-Authenticate")
	response.Body.Close()
	if strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		c.token = ""
		token, tokenErr := c.bearerToken(request.Context(), challenge)
		if tokenErr != nil {
			return nil, tokenErr
		}
		c.token = token
		retry, cloneErr := cloneRequestForRetry(request)
		if cloneErr != nil {
			return nil, cloneErr
		}
		retry.Header.Set("Authorization", "Bearer "+token)
		return c.http.Do(retry)
	}
	if strings.HasPrefix(strings.ToLower(challenge), "basic") && c.credentials.Username != "" {
		retry, cloneErr := cloneRequestForRetry(request)
		if cloneErr != nil {
			return nil, cloneErr
		}
		retry.SetBasicAuth(c.credentials.Username, c.credentials.Password)
		return c.http.Do(retry)
	}
	return nil, fmt.Errorf("registry requires authentication but did not provide a supported Bearer or Basic challenge")
}

func (c *registryClient) bearerToken(ctx context.Context, challenge string) (string, error) {
	parameters := parseAuthParameters(strings.TrimSpace(challenge[len("Bearer "):]))
	realm := parameters["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry Bearer challenge has no realm")
	}
	tokenURL, err := url.Parse(realm)
	if err != nil || (tokenURL.Scheme != "https" && tokenURL.Scheme != "http") || tokenURL.Host == "" {
		return "", fmt.Errorf("registry token realm is invalid")
	}
	if strings.HasPrefix(c.baseURL, "https://") && tokenURL.Scheme != "https" {
		return "", fmt.Errorf("refusing insecure HTTP token realm for HTTPS registry")
	}
	query := tokenURL.Query()
	if service := parameters["service"]; service != "" {
		query.Set("service", service)
	}
	scope := parameters["scope"]
	if c.scope != "" {
		scope = c.scope
	}
	if scope == "" {
		scope = "repository:" + c.repository + ":pull"
	}
	query.Set("scope", scope)
	tokenURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", err
	}
	if c.credentials.Username != "" {
		registryURL, _ := url.Parse(c.baseURL)
		registryHost := strings.ToLower(registryURL.Hostname())
		tokenHost := strings.ToLower(tokenURL.Hostname())
		trusted := registryHost == tokenHost || (registryHost == "registry-1.docker.io" && tokenHost == "auth.docker.io")
		if !trusted {
			return "", fmt.Errorf("refusing to send registry credentials to untrusted token host %q", tokenHost)
		}
		request.SetBasicAuth(c.credentials.Username, c.credentials.Password)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("request registry access token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", responseError(response)
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		return "", fmt.Errorf("decode registry access token: %w", err)
	}
	if payload.Token == "" {
		payload.Token = payload.AccessToken
	}
	if payload.Token == "" {
		return "", fmt.Errorf("registry token response is empty")
	}
	return payload.Token, nil
}

// Push publishes a verified Boxr OCI manifest, config and rootfs layer to a
// registry. It uses the OCI monolithic upload flow and only pushes a single
// Boxr rootfs layer; arbitrary Docker/OCI layer graphs remain unsupported.
func Push(ctx context.Context, reference string, bundle PushBundle, credentials Credentials) error {
	parsed, err := ParseReference(reference)
	if err != nil {
		return err
	}
	if parsed.Digest != "" {
		return fmt.Errorf("image push requires a tag, not a digest reference")
	}
	if int64(len(bundle.Manifest)) > maxManifestBytes || len(bundle.Manifest) == 0 {
		return fmt.Errorf("image manifest is empty or exceeds %d bytes", maxManifestBytes)
	}
	var remote manifest
	if err := json.Unmarshal(bundle.Manifest, &remote); err != nil {
		return fmt.Errorf("decode local image manifest: %w", err)
	}
	if remote.SchemaVersion != 2 || remote.MediaType != "application/vnd.oci.image.manifest.v1+json" || len(remote.Layers) != 1 {
		return fmt.Errorf("only a schema-2 Boxr OCI manifest with one rootfs layer can be pushed")
	}
	if remote.Config.MediaType != "application/vnd.oci.image.config.v1+json" || remote.Layers[0].MediaType != boxrLayerMediaType {
		return fmt.Errorf("image manifest does not use the Boxr OCI config and rootfs layer media types")
	}
	if err := validateDescriptor(remote.Config, maxConfigBytes); err != nil {
		return fmt.Errorf("invalid config descriptor: %w", err)
	}
	if err := validateDescriptor(remote.Layers[0], maxLayerBytes); err != nil {
		return fmt.Errorf("invalid layer descriptor: %w", err)
	}
	if int64(len(bundle.Config)) != remote.Config.Size || digestBytes(bundle.Config) != remote.Config.Digest {
		return fmt.Errorf("local config does not match its OCI descriptor")
	}
	var config imageConfig
	if err := json.Unmarshal(bundle.Config, &config); err != nil {
		return fmt.Errorf("decode local image config: %w", err)
	}
	if config.OS != "linux" || config.Architecture == "" || config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != 1 {
		return fmt.Errorf("local image config has unsupported platform or rootfs metadata")
	}
	if !digestPattern.MatchString(config.RootFS.DiffIDs[0]) {
		return fmt.Errorf("local image config has an invalid rootfs diff_id")
	}
	layerInfo, err := os.Lstat(bundle.LayerPath)
	if err != nil {
		return err
	}
	if layerInfo.Mode()&os.ModeSymlink != 0 || !layerInfo.Mode().IsRegular() || layerInfo.Size() != remote.Layers[0].Size {
		return fmt.Errorf("local rootfs layer must be a regular file matching its descriptor size")
	}
	layerDigest, layerSize, err := digestFile(bundle.LayerPath, maxLayerBytes)
	if err != nil {
		return err
	}
	if layerDigest != remote.Layers[0].Digest || layerSize != remote.Layers[0].Size {
		return fmt.Errorf("local rootfs layer does not match its OCI descriptor")
	}
	diffID, err := uncompressedDigest(bundle.LayerPath)
	if err != nil {
		return err
	}
	if diffID != config.RootFS.DiffIDs[0] {
		return fmt.Errorf("local rootfs diff_id mismatch")
	}
	baseURL := "https://" + parsed.Registry
	if parsed.Registry == "localhost" || strings.HasPrefix(parsed.Registry, "localhost:") || strings.HasPrefix(parsed.Registry, "127.0.0.1:") {
		baseURL = "http://" + parsed.Registry
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	fetcher := registryClient{http: client, baseURL: baseURL, repository: parsed.Repository, credentials: credentials, scope: "repository:" + parsed.Repository + ":pull,push"}
	if err := fetcher.uploadBytes(ctx, remote.Config, bundle.Config); err != nil {
		return fmt.Errorf("upload image config: %w", err)
	}
	if err := fetcher.uploadFile(ctx, remote.Layers[0], bundle.LayerPath); err != nil {
		return fmt.Errorf("upload rootfs layer: %w", err)
	}
	manifestDigest := digestBytes(bundle.Manifest)
	endpoint := baseURL + "/v2/" + parsed.Repository + "/manifests/" + url.PathEscape(parsed.Tag)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(bundle.Manifest))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	response, err := fetcher.do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusAccepted {
		return responseError(response)
	}
	if publishedDigest := response.Header.Get("Docker-Content-Digest"); publishedDigest != "" && publishedDigest != manifestDigest {
		return fmt.Errorf("registry published manifest digest %s, expected %s", publishedDigest, manifestDigest)
	}
	return nil
}

func (c *registryClient) uploadBytes(ctx context.Context, descriptor Descriptor, data []byte) error {
	if int64(len(data)) != descriptor.Size || digestBytes(data) != descriptor.Digest {
		return fmt.Errorf("blob data does not match descriptor %s", descriptor.Digest)
	}
	return c.upload(ctx, descriptor, func(location *url.URL) (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, location.String(), bytes.NewReader(data))
		if err == nil {
			request.Header.Set("Content-Type", "application/octet-stream")
		}
		return request, err
	})
}

func (c *registryClient) uploadFile(ctx context.Context, descriptor Descriptor, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != descriptor.Size {
		return fmt.Errorf("blob file does not match descriptor size")
	}
	digest, size, err := digestFile(path, maxLayerBytes)
	if err != nil {
		return err
	}
	if digest != descriptor.Digest || size != descriptor.Size {
		return fmt.Errorf("blob file does not match descriptor digest")
	}
	return c.upload(ctx, descriptor, func(location *url.URL) (*http.Request, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, location.String(), file)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		request.ContentLength = size
		request.GetBody = func() (io.ReadCloser, error) { return os.Open(path) }
		request.Header.Set("Content-Type", "application/octet-stream")
		return request, nil
	})
}

func (c *registryClient) upload(ctx context.Context, descriptor Descriptor, buildRequest func(*url.URL) (*http.Request, error)) error {
	start, err := url.Parse(c.baseURL + "/v2/" + c.repository + "/blobs/uploads/")
	if err != nil {
		return err
	}
	query := start.Query()
	query.Set("mount", descriptor.Digest)
	query.Set("from", c.repository)
	start.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, start.String(), nil)
	if err != nil {
		return err
	}
	response, err := c.do(request)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusCreated {
		publishedDigest := response.Header.Get("Docker-Content-Digest")
		_ = response.Body.Close()
		if publishedDigest != "" && publishedDigest != descriptor.Digest {
			return fmt.Errorf("registry mounted unexpected blob digest %s", publishedDigest)
		}
		return nil
	}
	if response.StatusCode != http.StatusAccepted {
		defer response.Body.Close()
		return responseError(response)
	}
	locationHeader := response.Header.Get("Location")
	_ = response.Body.Close()
	if locationHeader == "" {
		return fmt.Errorf("registry upload response omitted Location")
	}
	locationRef, err := url.Parse(locationHeader)
	if err != nil {
		return fmt.Errorf("invalid registry upload Location: %w", err)
	}
	base, _ := url.Parse(c.baseURL + "/")
	location := base.ResolveReference(locationRef)
	if !sameOrigin(base, location) {
		return fmt.Errorf("refusing registry upload Location on a different origin")
	}
	query = location.Query()
	query.Set("digest", descriptor.Digest)
	location.RawQuery = query.Encode()
	putRequest, err := buildRequest(location)
	if err != nil {
		return err
	}
	response, err = c.do(putRequest)
	if err != nil {
		if putRequest.Body != nil {
			_ = putRequest.Body.Close()
		}
		return err
	}
	defer response.Body.Close()
	if putRequest.Body != nil {
		_ = putRequest.Body.Close()
	}
	if response.StatusCode != http.StatusCreated {
		return responseError(response)
	}
	if publishedDigest := response.Header.Get("Docker-Content-Digest"); publishedDigest != "" && publishedDigest != descriptor.Digest {
		return fmt.Errorf("registry stored unexpected blob digest %s", publishedDigest)
	}
	return nil
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func cloneRequestForRetry(request *http.Request) (*http.Request, error) {
	retry := request.Clone(request.Context())
	retry.Header = request.Header.Clone()
	if request.GetBody != nil {
		body, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		retry.Body = body
	} else if request.Body != nil && request.Body != http.NoBody {
		return nil, fmt.Errorf("cannot retry registry request with a non-replayable body")
	}
	return retry, nil
}

func parseAuthParameters(value string) map[string]string {
	result := make(map[string]string)
	for len(value) > 0 {
		value = strings.TrimSpace(strings.TrimLeft(value, ","))
		key, rest, ok := strings.Cut(value, "=")
		if !ok {
			break
		}
		key = strings.TrimSpace(key)
		rest = strings.TrimSpace(rest)
		var parsed string
		if strings.HasPrefix(rest, "\"") {
			rest = rest[1:]
			end := strings.IndexByte(rest, '"')
			if end < 0 {
				break
			}
			parsed, value = rest[:end], rest[end+1:]
		} else {
			parsed, value, _ = strings.Cut(rest, ",")
		}
		result[key] = parsed
	}
	return result
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestFile(path string, limit int64) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return "", 0, fmt.Errorf("file is not a regular file within the %d-byte limit", limit)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, limit+1))
	if err != nil {
		return "", 0, err
	}
	if written > limit || written != info.Size() {
		return "", 0, fmt.Errorf("file size changed or exceeds the %d-byte limit", limit)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), written, nil
}

func uncompressedDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return "", fmt.Errorf("layer is not valid gzip: %w", err)
	}
	defer compressed.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, compressed); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func validateDescriptor(descriptor Descriptor, limit int64) error {
	if !digestPattern.MatchString(descriptor.Digest) || descriptor.Size < 0 || descriptor.Size > limit {
		return fmt.Errorf("invalid digest or size")
	}
	return nil
}

func isIndex(mediaType string) bool {
	return strings.Contains(mediaType, "image.index") || strings.Contains(mediaType, "manifest.list")
}
func looksLikeIndex(data []byte) bool {
	return strings.Contains(string(data[:min(len(data), 256)]), `"manifests"`)
}
func normalizeMediaType(value string) string {
	value, _, _ = strings.Cut(value, ";")
	return strings.TrimSpace(value)
}

func responseError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("registry request failed (%d): %s", response.StatusCode, message)
}
