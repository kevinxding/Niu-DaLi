package vertex

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"

	"github.com/golang-jwt/jwt/v5"

	"fmt"
	"time"
)

// Credentials is a Vertex AI credential JSON: a service account key, or gcloud
// user credentials ("type": "authorized_user") that carry a refresh token.
type Credentials struct {
	Type           string `json:"type,omitempty"`
	ProjectID      string `json:"project_id"`
	PrivateKeyID   string `json:"private_key_id"`
	PrivateKey     string `json:"private_key"`
	ClientEmail    string `json:"client_email"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret,omitempty"`
	RefreshToken   string `json:"refresh_token,omitempty"`
	QuotaProjectID string `json:"quota_project_id,omitempty"`
}

// CredentialTypeAuthorizedUser marks gcloud user credentials, the file written
// by `gcloud auth application-default login`.
const CredentialTypeAuthorizedUser = "authorized_user"

// googleOAuthTokenURL is where user credentials trade their refresh token for
// an access token; tests point it at a local server.
var googleOAuthTokenURL = "https://oauth2.googleapis.com/token"

// ParseCredentials decodes one credential JSON. User credentials name their
// project in quota_project_id, which then serves as the project.
func ParseCredentials(raw string) (Credentials, error) {
	var creds Credentials
	if err := common.UnmarshalJsonStr(raw, &creds); err != nil {
		return Credentials{}, fmt.Errorf("failed to decode credentials file: %w", err)
	}
	if creds.ProjectID == "" {
		creds.ProjectID = creds.QuotaProjectID
	}
	return creds, nil
}

// ValidateUserCredentials reports what gcloud user credentials lack to refresh
// a token and address a project.
func (creds Credentials) ValidateUserCredentials() error {
	if creds.Type != CredentialTypeAuthorizedUser {
		return fmt.Errorf("SDK credentials must be gcloud user credentials with \"type\": \"authorized_user\", got %q", creds.Type)
	}
	if creds.ClientID == "" || creds.ClientSecret == "" || creds.RefreshToken == "" {
		return errors.New("SDK credentials need client_id, client_secret and refresh_token")
	}
	if creds.ProjectID == "" {
		return errors.New("SDK credentials need quota_project_id (gcloud auth application-default set-quota-project PROJECT_ID) or project_id")
	}
	return nil
}

// refreshUserAccessToken trades a gcloud refresh token for an access token.
// Errors carry Google's error code and description, never the credentials.
func refreshUserAccessToken(client *http.Client, creds Credentials) (string, error) {
	if err := creds.ValidateUserCredentials(); err != nil {
		return "", err
	}
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("client_id", creds.ClientID)
	data.Set("client_secret", creds.ClientSecret)
	data.Set("refresh_token", creds.RefreshToken)
	resp, err := client.PostForm(googleOAuthTokenURL, data)
	if err != nil {
		return "", fmt.Errorf("failed to refresh user access token: %w", err)
	}
	defer resp.Body.Close()
	var result struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := common.DecodeJson(resp.Body, &result); err != nil {
		return "", fmt.Errorf("failed to decode token response (status %d): %w", resp.StatusCode, err)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("failed to refresh user access token (status %d): %s: %s", resp.StatusCode, result.Error, result.ErrorDescription)
	}
	return result.AccessToken, nil
}

// accessTokenTTL keeps cached access tokens well inside Google's one-hour
// token lifetime.
const accessTokenTTL = 30 * time.Minute

type cachedAccessToken struct {
	token     string
	expiresAt time.Time
}

// accessTokens caches access tokens by a hash of the credential JSON, so a key
// that is edited, deleted or moved to another index never reuses another
// credential's token.
var accessTokens sync.Map

func getAccessToken(a *Adaptor, info *relaycommon.RelayInfo) (string, error) {
	sum := sha256.Sum256([]byte(info.ApiKey))
	cacheKey := hex.EncodeToString(sum[:])
	if cached, ok := accessTokens.Load(cacheKey); ok {
		entry := cached.(cachedAccessToken)
		if time.Now().Before(entry.expiresAt) {
			return entry.token, nil
		}
	}

	var newToken string
	if a.AccountCredentials.Type == CredentialTypeAuthorizedUser {
		client, err := service.GetHttpClientWithProxySettings(info.ChannelSetting.Proxy, info.ChannelSetting)
		if err != nil {
			return "", fmt.Errorf("new proxy http client failed: %w", err)
		}
		newToken, err = refreshUserAccessToken(client, a.AccountCredentials)
		if err != nil {
			return "", err
		}
	} else {
		signedJWT, err := createSignedJWT(a.AccountCredentials.ClientEmail, a.AccountCredentials.PrivateKey)
		if err != nil {
			return "", fmt.Errorf("failed to create signed JWT: %w", err)
		}
		newToken, err = exchangeJwtForAccessToken(signedJWT, info)
		if err != nil {
			return "", fmt.Errorf("failed to exchange JWT for access token: %w", err)
		}
	}
	accessTokens.Store(cacheKey, cachedAccessToken{token: newToken, expiresAt: time.Now().Add(accessTokenTTL)})
	return newToken, nil
}

func createSignedJWT(email, privateKeyPEM string) (string, error) {

	privateKeyPEM = strings.ReplaceAll(privateKeyPEM, "-----BEGIN PRIVATE KEY-----", "")
	privateKeyPEM = strings.ReplaceAll(privateKeyPEM, "-----END PRIVATE KEY-----", "")
	privateKeyPEM = strings.ReplaceAll(privateKeyPEM, "\r", "")
	privateKeyPEM = strings.ReplaceAll(privateKeyPEM, "\n", "")
	privateKeyPEM = strings.ReplaceAll(privateKeyPEM, "\\n", "")

	block, _ := pem.Decode([]byte("-----BEGIN PRIVATE KEY-----\n" + privateKeyPEM + "\n-----END PRIVATE KEY-----"))
	if block == nil {
		return "", fmt.Errorf("failed to parse PEM block containing the private key")
	}

	privateKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}

	rsaPrivateKey, ok := privateKey.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("not an RSA private key")
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   email,
		"scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud":   "https://www.googleapis.com/oauth2/v4/token",
		"exp":   now.Add(time.Minute * 35).Unix(),
		"iat":   now.Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signedToken, err := token.SignedString(rsaPrivateKey)
	if err != nil {
		return "", err
	}

	return signedToken, nil
}

func exchangeJwtForAccessToken(signedJWT string, info *relaycommon.RelayInfo) (string, error) {

	authURL := "https://www.googleapis.com/oauth2/v4/token"
	data := url.Values{}
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	data.Set("assertion", signedJWT)

	client, err := service.GetHttpClientWithProxySettings(info.ChannelSetting.Proxy, info.ChannelSetting)
	if err != nil {
		return "", fmt.Errorf("new proxy http client failed: %w", err)
	}

	resp, err := client.PostForm(authURL, data)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if accessToken, ok := result["access_token"].(string); ok {
		return accessToken, nil
	}

	return "", fmt.Errorf("failed to get access token: %v", result)
}

func AcquireAccessToken(creds Credentials, proxy string) (string, error) {
	if creds.Type == CredentialTypeAuthorizedUser {
		client, err := service.GetHttpClientWithProxySettings(proxy, dto.ChannelSettings{})
		if err != nil {
			return "", fmt.Errorf("new proxy http client failed: %w", err)
		}
		return refreshUserAccessToken(client, creds)
	}
	signedJWT, err := createSignedJWT(creds.ClientEmail, creds.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("failed to create signed JWT: %w", err)
	}
	return exchangeJwtForAccessTokenWithProxy(signedJWT, proxy)
}

func exchangeJwtForAccessTokenWithProxy(signedJWT string, proxy string) (string, error) {
	authURL := "https://www.googleapis.com/oauth2/v4/token"
	data := url.Values{}
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	data.Set("assertion", signedJWT)

	var client *http.Client
	var err error
	if proxy != "" {
		client, err = service.GetHttpClientWithProxy(proxy)
		if err != nil {
			return "", fmt.Errorf("new proxy http client failed: %w", err)
		}
	} else {
		client = service.GetHttpClient()
	}

	resp, err := client.PostForm(authURL, data)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if accessToken, ok := result["access_token"].(string); ok {
		return accessToken, nil
	}
	return "", fmt.Errorf("failed to get access token: %v", result)
}
