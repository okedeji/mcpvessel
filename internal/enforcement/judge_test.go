package enforcement

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestJudgeServer(handler http.HandlerFunc) (*httptest.Server, *JudgeClient) {
	srv := httptest.NewServer(handler)
	client := NewJudgeClient(srv.URL, 0.7, "test-key", 5*time.Second)
	return srv, client
}

func respondJSON(w http.ResponseWriter, resp JudgeResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func TestJudge_SafeHighConfidence(t *testing.T) {
	srv, client := newTestJudgeServer(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, JudgeResponse{Results: []JudgeResult{
			{Safe: true, Confidence: 0.95, Reason: "benign read query"},
		}})
	})
	defer srv.Close()

	decision, reason, err := client.Evaluate("discovery", "sqli", "assess-1", "POST", "/api/users", []byte("SELECT 1"))
	require.NoError(t, err)
	assert.Equal(t, PayloadAllow, decision)
	assert.Equal(t, "benign read query", reason)
}

func TestJudge_DangerousHighConfidence(t *testing.T) {
	srv, client := newTestJudgeServer(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, JudgeResponse{Results: []JudgeResult{
			{Safe: false, Confidence: 0.9, Reason: "credential extraction"},
		}})
	})
	defer srv.Close()

	decision, reason, err := client.Evaluate("discovery", "sqli", "assess-1", "POST", "/api/users", []byte("UNION SELECT password FROM users"))
	require.NoError(t, err)
	assert.Equal(t, PayloadBlock, decision)
	assert.Equal(t, "credential extraction", reason)
}

func TestJudge_LowConfidence_Hold(t *testing.T) {
	srv, client := newTestJudgeServer(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, JudgeResponse{Results: []JudgeResult{
			{Safe: false, Confidence: 0.35, Reason: "uncertain intent"},
		}})
	})
	defer srv.Close()

	decision, reason, err := client.Evaluate("discovery", "sqli", "assess-1", "POST", "/api/users", []byte("1' OR 1=1--"))
	require.NoError(t, err)
	assert.Equal(t, PayloadHold, decision)
	assert.Equal(t, "uncertain intent", reason)
}

func TestJudge_Timeout_FailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	client := NewJudgeClient(srv.URL, 0.7, "", 200*time.Millisecond)
	decision, _, err := client.Evaluate("discovery", "sqli", "assess-1", "GET", "/api", nil)
	assert.Error(t, err)
	assert.Equal(t, PayloadBlock, decision)
}

func TestJudge_MalformedResponse_FailClosed(t *testing.T) {
	srv, client := newTestJudgeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})
	defer srv.Close()

	decision, _, err := client.Evaluate("discovery", "sqli", "assess-1", "GET", "/api", nil)
	assert.Error(t, err)
	assert.Equal(t, PayloadBlock, decision)
}

func TestJudge_WrongResultCount_FailClosed(t *testing.T) {
	srv, client := newTestJudgeServer(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, JudgeResponse{Results: []JudgeResult{
			{Safe: true, Confidence: 0.9, Reason: "ok"},
			{Safe: false, Confidence: 0.8, Reason: "bad"},
		}})
	})
	defer srv.Close()

	decision, _, err := client.Evaluate("discovery", "sqli", "assess-1", "GET", "/api", nil)
	assert.Error(t, err)
	assert.Equal(t, PayloadBlock, decision)
}

func TestJudge_InvalidConfidence_FailClosed(t *testing.T) {
	srv, client := newTestJudgeServer(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, JudgeResponse{Results: []JudgeResult{
			{Safe: true, Confidence: 1.5, Reason: "impossible"},
		}})
	})
	defer srv.Close()

	decision, _, err := client.Evaluate("discovery", "sqli", "assess-1", "GET", "/api", nil)
	assert.Error(t, err)
	assert.Equal(t, PayloadBlock, decision)
}

func TestJudge_AuthHeaderSent(t *testing.T) {
	var gotAuth string
	srv, client := newTestJudgeServer(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("x-api-key")
		respondJSON(w, JudgeResponse{Results: []JudgeResult{
			{Safe: true, Confidence: 0.9, Reason: "ok"},
		}})
	})
	defer srv.Close()

	_, _, err := client.Evaluate("discovery", "sqli", "assess-1", "GET", "/api", nil)
	require.NoError(t, err)
	assert.Equal(t, "test-key", gotAuth)
}

func TestJudge_RequestPayloadCorrect(t *testing.T) {
	var gotReq JudgeRequest
	srv, client := newTestJudgeServer(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		respondJSON(w, JudgeResponse{Results: []JudgeResult{
			{Safe: true, Confidence: 0.9, Reason: "ok"},
		}})
	})
	defer srv.Close()

	_, _, err := client.Evaluate("exploitation", "rce", "assess-42", "POST", "/exec", []byte("whoami"))
	require.NoError(t, err)
	require.Len(t, gotReq.Payloads, 1)
	assert.Equal(t, "exploitation", gotReq.Payloads[0].CageType)
	assert.Equal(t, "rce", gotReq.Payloads[0].VulnClass)
	assert.Equal(t, "assess-42", gotReq.Payloads[0].AssessmentID)
	assert.Equal(t, "POST", gotReq.Payloads[0].Method)
	assert.Equal(t, "/exec", gotReq.Payloads[0].URL)
	assert.Equal(t, "whoami", gotReq.Payloads[0].Body)
}

func TestJudge_ServerError_FailClosed(t *testing.T) {
	srv, client := newTestJudgeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()

	decision, _, err := client.Evaluate("discovery", "sqli", "assess-1", "GET", "/api", nil)
	assert.Error(t, err)
	assert.Equal(t, PayloadBlock, decision)
}
