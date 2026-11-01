package vertex

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userCredentialsJSON mirrors application_default_credentials.json written by
// `gcloud auth application-default login` plus set-quota-project.
const userCredentialsJSON = `{
  "account": "",
  "client_id": "gcloud-client.apps.googleusercontent.com",
  "client_secret": "client-secret-value",
  "quota_project_id": "quota-project",
  "refresh_token": "1//refresh-token-value",
  "type": "authorized_user",
  "universe_domain": "googleapis.com"
}`

func useTokenServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	original := googleOAuthTokenURL
	googleOAuthTokenURL = server.URL
	t.Cleanup(func() {
		googleOAuthTokenURL = original
		server.Close()
	})
}

func userCredentialRelayInfo(channelID int) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		OriginModelName: "gemini-2.5-flash",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:            channelID,
			ApiKey:               userCredentialsJSON,
			ApiVersion:           "global",
			UpstreamModelName:    "gemini-2.5-flash",
			ChannelOtherSettings: dto.ChannelOtherSettings{VertexKeyType: dto.VertexKeyTypeSDK},
		},
	}
}

func TestUserCredentialsAuthorizeRequestsWithRefreshedToken(t *testing.T) {
	var refreshes atomic.Int32
	useTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "refresh_token", r.PostForm.Get("grant_type"))
		assert.Equal(t, "gcloud-client.apps.googleusercontent.com", r.PostForm.Get("client_id"))
		assert.Equal(t, "client-secret-value", r.PostForm.Get("client_secret"))
		assert.Equal(t, "1//refresh-token-value", r.PostForm.Get("refresh_token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ya29.user-token","expires_in":3599,"token_type":"Bearer"}`))
	})
	accessTokens.Clear()
	t.Cleanup(accessTokens.Clear)
	info := userCredentialRelayInfo(970001)

	adaptor := &Adaptor{}
	adaptor.Init(info)
	requestURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://aiplatform.googleapis.com/v1/projects/quota-project/locations/global/publishers/google/models/gemini-2.5-flash:generateContent", requestURL)

	for range 2 {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		header := http.Header{}
		require.NoError(t, adaptor.SetupRequestHeader(c, &header, info))
		assert.Equal(t, "Bearer ya29.user-token", header.Get("Authorization"))
		assert.Equal(t, "quota-project", header.Get("x-goog-user-project"))
	}
	assert.Equal(t, int32(1), refreshes.Load(), "the access token is cached between requests")
}

func TestUserCredentialRefreshFailureReportsGoogleErrorWithoutSecrets(t *testing.T) {
	useTokenServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
	})
	creds, err := ParseCredentials(userCredentialsJSON)
	require.NoError(t, err)

	_, err = AcquireAccessToken(creds, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 400")
	assert.Contains(t, err.Error(), "invalid_grant: Token has been expired or revoked.")
	assert.NotContains(t, err.Error(), "refresh-token-value")
	assert.NotContains(t, err.Error(), "client-secret-value")
}

func TestValidateUserCredentials(t *testing.T) {
	withField := func(field string, value any) string {
		var fields map[string]any
		require.NoError(t, common.UnmarshalJsonStr(userCredentialsJSON, &fields))
		if value == nil {
			delete(fields, field)
		} else {
			fields[field] = value
		}
		raw, err := common.Marshal(fields)
		require.NoError(t, err)
		return string(raw)
	}
	tests := []struct {
		name    string
		raw     string
		project string
		wantErr string
	}{
		{name: "quota project names the project", raw: userCredentialsJSON, project: "quota-project"},
		{name: "explicit project_id wins over the quota project", raw: withField("project_id", "billing-project"), project: "billing-project"},
		{name: "service account keys are not SDK credentials", raw: withField("type", "service_account"), wantErr: `got "service_account"`},
		{name: "a missing refresh token is rejected", raw: withField("refresh_token", nil), wantErr: "need client_id, client_secret and refresh_token"},
		{name: "a missing project is rejected", raw: withField("quota_project_id", nil), wantErr: "need quota_project_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creds, err := ParseCredentials(tt.raw)
			require.NoError(t, err)

			err = creds.ValidateUserCredentials()

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.project, creds.ProjectID)
		})
	}
}
