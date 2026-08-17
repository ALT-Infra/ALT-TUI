package cline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"altv1/internal/credential"
)

func TestDeviceAuthorizationStoresRotatingAccountCredentialAndListsModels(t *testing.T) {
	t.Setenv("ALT_ALLOW_INSECURE_PROVIDER_ENDPOINT", "1")
	var registered atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/user_management/authorize/device":
			if err := request.ParseForm(); err != nil || request.Form.Get("client_id") != "test-client" {
				t.Fatalf("device form = %#v, err = %v", request.Form, err)
			}
			writeJSON(writer, map[string]any{
				"device_code": "device-1", "user_code": "CODE-1",
				"verification_uri":          "https://example.test/device",
				"verification_uri_complete": "https://example.test/device?code=CODE-1",
				"expires_in":                60, "interval": 1,
			})
		case "/user_management/authenticate":
			if err := request.ParseForm(); err != nil || request.Form.Get("device_code") != "device-1" {
				t.Fatalf("authentication form = %#v, err = %v", request.Form, err)
			}
			writeJSON(writer, map[string]any{
				"access_token": "workos-access", "refresh_token": "workos-refresh",
			})
		case "/api/v1/auth/register":
			registered.Add(1)
			writeJSON(writer, clineTokenResponse{Success: true, Data: clineTokenData{
				AccessToken: "cline-access", RefreshToken: "cline-refresh",
				ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			}})
		case "/api/v1/ai/cline/recommended-models":
			writeJSON(writer, map[string]any{
				"clinePass": []map[string]string{{"id": "cline-pass/kimi-k3", "name": "Kimi K3"}},
				"free":      []map[string]string{{"id": "deepseek/deepseek-v4-flash", "name": "DeepSeek V4 Flash"}},
			})
		case "/api/v1/users/me":
			if request.Header.Get("Authorization") != "Bearer workos:cline-access" {
				t.Fatalf("authorization header = %q", request.Header.Get("Authorization"))
			}
			writeJSON(writer, map[string]any{"data": []any{}})
		case "/api/v1/users/me/plan":
			writeJSON(writer, map[string]any{
				"success": true,
				"data": map[string]any{
					"plan":             map[string]string{"id": "pass"},
					"currentPeriodEnd": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	factory := testFactory(t, server.URL)
	authorization, err := factory.BeginDeviceAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if authorization.UserCode != "CODE-1" || authorization.VerificationURIComplete == "" {
		t.Fatalf("authorization = %#v", authorization)
	}
	if err := factory.CompleteDeviceAuthorization(context.Background(), authorization, nil); err != nil {
		t.Fatal(err)
	}
	if registered.Load() != 1 {
		t.Fatalf("registration count = %d", registered.Load())
	}
	token, err := factory.resolveAccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "workos:cline-access" {
		t.Fatalf("token = %q", token)
	}
	models, err := factory.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[1].ID != "deepseek/deepseek-v4-flash" {
		t.Fatalf("models = %#v", models)
	}
}

func TestDeviceAuthorizationTimingComesFromRFCAndServer(t *testing.T) {
	t.Setenv("ALT_ALLOW_INSECURE_PROVIDER_ENDPOINT", "1")
	var omitExpiry atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload := map[string]any{
			"device_code": "device-rfc", "user_code": "RFC-CODE",
			"verification_uri": "https://example.test/device",
		}
		if !omitExpiry.Load() {
			payload["expires_in"] = 90
		}
		writeJSON(writer, payload)
	}))
	defer server.Close()
	factory := testFactory(t, server.URL)
	authorization, err := factory.BeginDeviceAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if authorization.ExpiresInSeconds != 90 || authorization.PollIntervalSeconds != 5 {
		t.Fatalf("RFC/server device timing = %#v", authorization)
	}
	omitExpiry.Store(true)
	if _, err := factory.BeginDeviceAuthorization(context.Background()); err == nil || !strings.Contains(err.Error(), "expires_in") {
		t.Fatalf("missing server expiry was replaced with a client guess: %v", err)
	}
}

func TestCredentialRefreshLeadIsDerivedFromCallerLifetime(t *testing.T) {
	t.Setenv("ALT_ALLOW_INSECURE_PROVIDER_ENDPOINT", "1")
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		refreshes.Add(1)
		writeJSON(writer, clineTokenResponse{Success: true, Data: clineTokenData{
			AccessToken: "unexpected-refresh", RefreshToken: "rotated",
			ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}})
	}))
	defer server.Close()
	factory := testFactory(t, server.URL)
	credentialValue, _ := json.Marshal(storedCredentials{
		AccessToken: "still-valid", RefreshToken: "refresh-if-needed",
		ExpiresAt: time.Now().Add(4 * time.Minute).UTC().Format(time.RFC3339),
	})
	if _, err := factory.Credentials.Set(Name, string(credentialValue)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	token, err := factory.resolveAccessToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if token != "workos:still-valid" || refreshes.Load() != 0 {
		t.Fatalf("credential was refreshed using a fixed lead window: token=%q refreshes=%d", token, refreshes.Load())
	}
}

func TestExpiredCredentialRefreshIsSingleFlightAndPersistsRotation(t *testing.T) {
	t.Setenv("ALT_ALLOW_INSECURE_PROVIDER_ENDPOINT", "1")
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/auth/refresh" {
			http.NotFound(writer, request)
			return
		}
		refreshes.Add(1)
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["refreshToken"] != "old-refresh" || body["grantType"] != "refresh_token" {
			t.Fatalf("refresh body = %#v", body)
		}
		writeJSON(writer, clineTokenResponse{Success: true, Data: clineTokenData{
			AccessToken: "new-access", RefreshToken: "new-refresh",
			ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}})
	}))
	defer server.Close()

	factory := testFactory(t, server.URL)
	expired, _ := json.Marshal(storedCredentials{
		AccessToken: "old-access", RefreshToken: "old-refresh",
		ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	if _, err := factory.Credentials.Set(Name, string(expired)); err != nil {
		t.Fatal(err)
	}

	const callers = 12
	var wait sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			token, err := factory.resolveAccessToken(context.Background())
			if err == nil && token != "workos:new-access" {
				err = &unexpectedToken{token: token}
			}
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh count = %d, want 1", refreshes.Load())
	}
	raw, err := factory.Credentials.Resolve(Name, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "new-refresh") || strings.Contains(raw, "old-refresh") {
		t.Fatalf("stored credential was not rotated: %s", raw)
	}
}

func TestInferenceTransportAddsClineHeadersWithoutRewritingLanguageResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Client-Type") != "alt" || request.Header.Get("X-Task-Id") != "task-1" {
			t.Fatalf("headers = %#v", request.Header)
		}
		writer.Header().Set("Content-Type", "application/json")
		writeJSON(writer, map[string]any{
			"success": true,
			"data":    map[string]any{"choices": []map[string]any{{"index": 0}}},
		})
	}))
	defer server.Close()
	transport := &headerTransport{
		base:    http.DefaultTransport,
		headers: clineRequestHeaders("task-1"),
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("{}"))
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["data"] == nil || payload["success"] == nil || payload["choices"] != nil {
		t.Fatalf("language response was rewritten as an image envelope: %#v", payload)
	}
}

func TestNoPlanHistoryMeansFreeCatalogOnly(t *testing.T) {
	t.Setenv("ALT_ALLOW_INSECURE_PROVIDER_ENDPOINT", "1")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/users/me/plan" {
			http.NotFound(writer, request)
			return
		}
		writer.WriteHeader(http.StatusNotFound)
		writeJSON(writer, map[string]any{"success": false, "error": "no plan history found for user"})
	}))
	defer server.Close()
	factory := testFactory(t, server.URL)
	hasPass, err := factory.hasActivePass(context.Background(), "workos:test")
	if err != nil {
		t.Fatal(err)
	}
	if hasPass {
		t.Fatal("account without plan history was treated as subscribed")
	}
}

type unexpectedToken struct{ token string }

func (e *unexpectedToken) Error() string { return "unexpected token: " + e.token }

func testFactory(t *testing.T, endpoint string) *Factory {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	_ = parsed
	factory := NewFactory(credential.NewStore(t.TempDir()))
	factory.APIBaseURL = endpoint
	factory.WorkOSBaseURL = endpoint
	factory.WorkOSClientID = "test-client"
	factory.HTTPClient = &http.Client{Transport: http.DefaultTransport}
	return factory
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}
