package cline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"altv1/internal/buildinfo"
	"altv1/internal/credential"
	"altv1/internal/profile"
	"altv1/internal/provider"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/google/uuid"
)

const (
	Name              = "cline"
	Route             = "pass"
	DefaultAPIBase    = "https://api.cline.bot"
	DefaultWorkOSBase = "https://api.workos.com"
	// WorkOS OAuth client identifiers are public identifiers, not credentials.
	// This is Cline's production device client as published by its open-source SDK.
	DefaultWorkOSClientID = "client_01K3A541FN8TA3EPPHTD2325AR"
	credentialEnvironment = "ALT_CLINE_API_KEY"
	refreshBuffer         = 5 * time.Minute
)

type Factory struct {
	Credentials    credential.Store
	HTTPClient     *http.Client
	APIBaseURL     string
	WorkOSBaseURL  string
	WorkOSClientID string

	mu sync.Mutex
}

func NewFactory(credentials credential.Store) *Factory {
	return &Factory{
		Credentials:    credentials,
		HTTPClient:     &http.Client{Transport: http.DefaultTransport},
		APIBaseURL:     DefaultAPIBase,
		WorkOSBaseURL:  DefaultWorkOSBase,
		WorkOSClientID: DefaultWorkOSClientID,
	}
}

func (*Factory) Descriptor() provider.GatewayDescriptor {
	return provider.GatewayDescriptor{
		ID:                    Name,
		Name:                  "ClinePass",
		CredentialEnvironment: credentialEnvironment,
		Authentication:        provider.AuthenticationDeviceOAuth,
		MultiModelCatalog:     true,
		Routes:                []provider.GatewayRoute{{ID: Route, Label: "ClinePass + Free"}},
	}
}

func (*Factory) Capabilities(profile.Model) provider.Capabilities {
	return provider.Capabilities{
		StructuredOutput: provider.CapabilityUnknown,
		ToolCalling:      provider.CapabilitySupported,
	}
}

type storedCredentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
}

type clineUser struct {
	Subject     string   `json:"subject"`
	Email       string   `json:"email"`
	Name        string   `json:"name"`
	ClineUserID string   `json:"clineUserId"`
	Accounts    []string `json:"accounts"`
}

type clineTokenData struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	TokenType    string    `json:"tokenType"`
	ExpiresAt    string    `json:"expiresAt"`
	UserInfo     clineUser `json:"userInfo"`
}

type clineTokenResponse struct {
	Success bool           `json:"success"`
	Data    clineTokenData `json:"data"`
}

type workOSDeviceResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	Error                   string `json:"error"`
	ErrorDescription        string `json:"error_description"`
}

type workOSTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (f *Factory) BeginDeviceAuthorization(ctx context.Context) (provider.DeviceAuthorization, error) {
	endpoint := strings.TrimRight(f.WorkOSBaseURL, "/") + "/user_management/authorize/device"
	if err := validateEndpoint("Cline WorkOS", endpoint, "api.workos.com"); err != nil {
		return provider.DeviceAuthorization{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(url.Values{"client_id": {f.WorkOSClientID}}.Encode()))
	if err != nil {
		return provider.DeviceAuthorization{}, fmt.Errorf("create Cline device authorization request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var payload workOSDeviceResponse
	if err := f.doJSON(request, &payload); err != nil {
		return provider.DeviceAuthorization{}, fmt.Errorf("begin Cline device authorization: %w", err)
	}
	if payload.DeviceCode == "" || payload.UserCode == "" || payload.VerificationURI == "" {
		return provider.DeviceAuthorization{}, errors.New("Cline returned an incomplete device authorization")
	}
	if payload.ExpiresIn <= 0 {
		payload.ExpiresIn = 300
	}
	if payload.Interval <= 0 {
		payload.Interval = 5
	}
	return provider.DeviceAuthorization{
		VerificationURI:         payload.VerificationURI,
		VerificationURIComplete: payload.VerificationURIComplete,
		UserCode:                payload.UserCode,
		DeviceCode:              payload.DeviceCode,
		ExpiresInSeconds:        payload.ExpiresIn,
		PollIntervalSeconds:     payload.Interval,
	}, nil
}

func (f *Factory) CompleteDeviceAuthorization(
	ctx context.Context,
	authorization provider.DeviceAuthorization,
	progress func(string),
) error {
	deadline := time.Now().Add(time.Duration(authorization.ExpiresInSeconds) * time.Second)
	interval := time.Duration(max(1, authorization.PollIntervalSeconds)) * time.Second
	endpoint := strings.TrimRight(f.WorkOSBaseURL, "/") + "/user_management/authenticate"
	if err := validateEndpoint("Cline WorkOS", endpoint, "api.workos.com"); err != nil {
		return err
	}
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
			strings.NewReader(url.Values{
				"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
				"device_code": {authorization.DeviceCode},
				"client_id":   {f.WorkOSClientID},
			}.Encode()))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := f.HTTPClient.Do(request)
		if err != nil {
			return fmt.Errorf("poll Cline device authorization: %w", err)
		}
		var tokens workOSTokenResponse
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&tokens)
		response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if decodeErr != nil || tokens.AccessToken == "" || tokens.RefreshToken == "" {
				return errors.New("Cline returned an incomplete device token")
			}
			return f.registerAndStore(ctx, tokens)
		}
		switch tokens.Error {
		case "authorization_pending":
			if progress != nil {
				progress("waiting for browser confirmation")
			}
		case "slow_down":
			interval += time.Second
		case "access_denied", "expired_token", "invalid_grant":
			return fmt.Errorf("Cline authorization failed: %s", firstNonEmpty(tokens.ErrorDescription, tokens.Error))
		default:
			return fmt.Errorf("Cline authorization failed: HTTP %d%s", response.StatusCode, errorSuffix(tokens.ErrorDescription))
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("Cline device authorization timed out")
}

func (f *Factory) registerAndStore(ctx context.Context, tokens workOSTokenResponse) error {
	endpoint := strings.TrimRight(f.APIBaseURL, "/") + "/api/v1/auth/register"
	var payload clineTokenResponse
	if err := f.postJSON(ctx, endpoint, "api.cline.bot", map[string]string{
		"accessToken":  tokens.AccessToken,
		"refreshToken": tokens.RefreshToken,
	}, &payload); err != nil {
		return fmt.Errorf("register Cline account token: %w", err)
	}
	if !payload.Success || payload.Data.AccessToken == "" ||
		payload.Data.RefreshToken == "" || payload.Data.ExpiresAt == "" {
		return errors.New("Cline returned incomplete account credentials")
	}
	return f.store(payload.Data)
}

func (f *Factory) store(data clineTokenData) error {
	encoded, err := json.Marshal(storedCredentials{
		AccessToken:  data.AccessToken,
		RefreshToken: data.RefreshToken,
		ExpiresAt:    data.ExpiresAt,
	})
	if err != nil {
		return err
	}
	_, err = f.Credentials.Set(Name, string(encoded))
	return err
}

func (f *Factory) resolveAccessToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := f.Credentials.Resolve(Name, credentialEnvironment)
	if err != nil {
		return "", err
	}
	var stored storedCredentials
	if json.Unmarshal([]byte(raw), &stored) != nil || stored.AccessToken == "" {
		return addWorkOSPrefix(raw), nil
	}
	expiresAt, parseErr := time.Parse(time.RFC3339, stored.ExpiresAt)
	if parseErr != nil {
		return "", fmt.Errorf("read Cline credential expiry: %w", parseErr)
	}
	if time.Until(expiresAt) > refreshBuffer {
		return addWorkOSPrefix(stored.AccessToken), nil
	}
	if stored.RefreshToken == "" {
		return "", errors.New("Cline credentials expired without a refresh token; authenticate again")
	}
	endpoint := strings.TrimRight(f.APIBaseURL, "/") + "/api/v1/auth/refresh"
	var payload clineTokenResponse
	if err := f.postJSON(ctx, endpoint, "api.cline.bot", map[string]string{
		"refreshToken": stored.RefreshToken,
		"grantType":    "refresh_token",
	}, &payload); err != nil {
		return "", fmt.Errorf("refresh Cline account token: %w", err)
	}
	if !payload.Success || payload.Data.AccessToken == "" || payload.Data.ExpiresAt == "" {
		return "", errors.New("Cline returned an incomplete refreshed credential")
	}
	if payload.Data.RefreshToken == "" {
		payload.Data.RefreshToken = stored.RefreshToken
	}
	if err := f.store(payload.Data); err != nil {
		return "", fmt.Errorf("persist refreshed Cline credential: %w", err)
	}
	return addWorkOSPrefix(payload.Data.AccessToken), nil
}

func (f *Factory) NewChatModel(ctx context.Context, spec profile.Model, mode provider.Mode) (model.BaseChatModel, error) {
	if strings.TrimSpace(spec.Route) != Route {
		return nil, fmt.Errorf("unknown ClinePass catalog route %q", spec.Route)
	}
	baseURL := strings.TrimRight(f.APIBaseURL, "/") + "/api/v1"
	if err := validateEndpoint("Cline", baseURL, "api.cline.bot"); err != nil {
		return nil, err
	}
	key, err := f.resolveAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	client := *f.HTTPClient
	client.Transport = &headerTransport{
		base:    f.HTTPClient.Transport,
		headers: clineRequestHeaders(uuid.NewString()),
	}
	config := &einoopenai.ChatModelConfig{
		APIKey:     key,
		BaseURL:    baseURL,
		Model:      spec.Name,
		HTTPClient: &client,
	}
	_ = mode
	if spec.ReasoningEffort != "" {
		config.ExtraFields = map[string]any{"reasoning_effort": spec.ReasoningEffort}
	}
	return einoopenai.NewChatModel(ctx, config)
}

type headerTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (transport *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for key, values := range transport.headers {
		if clone.Header.Get(key) != "" {
			continue
		}
		for _, value := range values {
			clone.Header.Add(key, value)
		}
	}
	base := transport.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 ||
		!strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		return response, nil
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read Cline response: %w", err)
	}
	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Success && len(envelope.Data) > 0 {
		body = envelope.Data
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	return response, nil
}

func clineRequestHeaders(taskID string) http.Header {
	version := strings.TrimSpace(buildinfo.Version)
	if version == "" {
		version = "dev"
	}
	return http.Header{
		"HTTP-Referer":       {"https://cline.bot"},
		"X-Title":            {"ALT"},
		"X-Is-Multiroot":     {"false"},
		"X-Client-Type":      {"alt"},
		"X-Client-Version":   {version},
		"X-Platform":         {runtime.GOOS},
		"X-Platform-Version": {runtime.GOARCH},
		"X-Core-Version":     {version},
		"X-Task-Id":          {taskID},
		"User-Agent":         {"ALT/" + version},
	}
}

type recommendedModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type recommendedModelsResponse struct {
	ClinePass []recommendedModel `json:"clinePass"`
	Free      []recommendedModel `json:"free"`
}

func (f *Factory) ListModels(ctx context.Context) ([]provider.CatalogModel, error) {
	// Resolving first proves that the configured account credential is usable
	// and rotates it when necessary. An authenticated, zero-inference models
	// request then rejects revoked or malformed credentials before ALT accepts
	// the public current pass/free catalog.
	token, err := f.resolveAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	if err := f.validateAccount(ctx, token); err != nil {
		return nil, fmt.Errorf("authenticate Cline account: %w", err)
	}
	hasPass, err := f.hasActivePass(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("inspect ClinePass entitlement: %w", err)
	}
	endpoint := strings.TrimRight(f.APIBaseURL, "/") + "/api/v1/ai/cline/recommended-models"
	if err := validateEndpoint("Cline", endpoint, "api.cline.bot"); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	var payload recommendedModelsResponse
	if err := f.doJSON(request, &payload); err != nil {
		return nil, fmt.Errorf("list ClinePass models: %w", err)
	}
	seen := map[string]bool{}
	available := payload.Free
	if hasPass {
		available = append(payload.ClinePass, available...)
	}
	result := make([]provider.CatalogModel, 0, len(available))
	for _, item := range available {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, provider.CatalogModel{
			Gateway:     Name,
			Route:       Route,
			ID:          id,
			DisplayName: strings.TrimSpace(item.Name),
			Capabilities: provider.Capabilities{
				StructuredOutput: provider.CapabilityUnknown,
				ToolCalling:      provider.CapabilitySupported,
			},
		})
	}
	if len(result) == 0 {
		return nil, errors.New("Cline returned no ClinePass or free models")
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (f *Factory) hasActivePass(ctx context.Context, token string) (bool, error) {
	endpoint := strings.TrimRight(f.APIBaseURL, "/") + "/api/v1/users/me/plan"
	if err := validateEndpoint("Cline", endpoint, "api.cline.bot"); err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := f.HTTPClient.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return false, err
	}
	if response.StatusCode == http.StatusNotFound &&
		strings.Contains(strings.ToLower(string(body)), "no plan") {
		return false, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			Plan             json.RawMessage `json:"plan"`
			CurrentPeriodEnd string          `json:"currentPeriodEnd"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false, fmt.Errorf("decode Cline plan: %w", err)
	}
	if !envelope.Success || len(envelope.Data.Plan) == 0 || string(envelope.Data.Plan) == "null" {
		return false, nil
	}
	if envelope.Data.CurrentPeriodEnd != "" {
		periodEnd, parseErr := time.Parse(time.RFC3339, envelope.Data.CurrentPeriodEnd)
		if parseErr == nil && time.Now().After(periodEnd) {
			return false, nil
		}
	}
	return true, nil
}

func (f *Factory) validateAccount(ctx context.Context, token string) error {
	endpoint := strings.TrimRight(f.APIBaseURL, "/") + "/api/v1/users/me"
	if err := validateEndpoint("Cline", endpoint, "api.cline.bot"); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	var payload json.RawMessage
	return f.doJSON(request, &payload)
}

func (f *Factory) postJSON(ctx context.Context, endpoint, hostname string, body any, target any) error {
	if err := validateEndpoint("Cline", endpoint, hostname); err != nil {
		return err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(encoded)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return f.doJSON(request, target)
}

func (f *Factory) doJSON(request *http.Request, target any) error {
	response, err := f.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		var details struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &details)
		message := firstNonEmpty(details.Message, details.Error)
		return fmt.Errorf("HTTP %d%s", response.StatusCode, errorSuffix(message))
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func validateEndpoint(name, raw, hostname string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %s endpoint: %w", name, err)
	}
	if os.Getenv("ALT_ALLOW_INSECURE_PROVIDER_ENDPOINT") == "1" {
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("%s endpoint must use HTTP or HTTPS", name)
		}
		return nil
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%s endpoint must use HTTPS", name)
	}
	if parsed.Hostname() != hostname {
		return fmt.Errorf("refusing to send a %s credential to %s", name, parsed.Hostname())
	}
	return nil
}

func addWorkOSPrefix(token string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(token), "workos:") {
		return token
	}
	return "workos:" + token
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func errorSuffix(message string) string {
	if strings.TrimSpace(message) == "" {
		return ""
	}
	return ": " + strings.TrimSpace(message)
}
