package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"verdent2api/internal/pool"
	"verdent2api/internal/upstream"
)

// fakeUpstream 落地一个假上游，按用例脚本返回 SSE / JSON / 错误。
type fakeUpstream struct {
	status     int
	body       string
	contentType string
	lastBody   string
	lastAuth   string
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", f.contentType)
	if f.status != 0 {
		w.WriteHeader(f.status)
	}
	b, _ := io.ReadAll(r.Body)
	f.lastBody = string(b)
	f.lastAuth = r.Header.Get("Authorization")
	_, _ = w.Write([]byte(f.body))
}

type staticKeys struct{ keys []string }

func (s *staticKeys) Keys() []string { return s.keys }

func newTestServer(up *fakeUpstream, gwKey string) (*Server, *httptest.Server) {
	upSrv := httptest.NewServer(up)
	client := upstream.New(upSrv.URL, 5*time.Second, 5*time.Second, 5*time.Second, "")
	p := pool.New(pool.Config{SelectJitterMS: 0}, client, &staticKeys{keys: []string{"sk-test"}}, "")
	return &Server{
		Pool:     p,
		Client:   client,
		APIKey:   gwKey,
		Version:  "test",
	}, upSrv
}

func do(s *Server, method, path, apiKey, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAuth_Required(t *testing.T) {
	up := &fakeUpstream{}
	s, upSrv := newTestServer(up, "gw-secret")
	defer upSrv.Close()

	rec := do(s, http.MethodPost, "/v1/chat/completions", "wrong", `{"model":"m","messages":[]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var parsed map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	errObj, ok := parsed["error"].(map[string]interface{})
	if !ok || errObj["message"] == nil {
		t.Fatalf("missing OpenAI-style error envelope: %v", parsed)
	}
}

func TestAuth_OptionalWhenEmpty(t *testing.T) {
	up := &fakeUpstream{contentType: "application/json", body: `{"id":"1","object":"chat.completion","model":"m"}`}
	s, upSrv := newTestServer(up, "")
	defer upSrv.Close()

	rec := do(s, http.MethodPost, "/v1/chat/completions", "", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(up.lastAuth, "Bearer sk-test") {
		t.Fatalf("upstream should get pool key, got %q", up.lastAuth)
	}
}

func TestChat_StreamPassthrough(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		"data: [DONE]\n\n"
	up := &fakeUpstream{contentType: "text/event-stream", body: sse}
	s, upSrv := newTestServer(up, "")
	defer upSrv.Close()

	rec := do(s, http.MethodPost, "/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("missing DONE frame: %s", out)
	}
	if !strings.Contains(out, `"content":"hel"`) || !strings.Contains(out, `"content":"lo"`) {
		t.Fatalf("content not streamed: %s", out)
	}
	// 规范化后应带标准 object 字段。
	if !strings.Contains(out, `"chat.completion.chunk"`) {
		t.Fatalf("chunks not normalized: %s", out)
	}
	if !strings.HasPrefix(out, "data: ") {
		t.Fatalf("first line should be an SSE data line: %q", out)
	}
}

func TestChat_UpstreamErrorMapped(t *testing.T) {
	up := &fakeUpstream{
		status:      http.StatusTooManyRequests,
		contentType: "application/json",
		body:        `{"error":{"message":"rate limited","type":"rate_limit"}}`,
	}
	s, upSrv := newTestServer(up, "")
	defer upSrv.Close()

	rec := do(s, http.MethodPost, "/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var parsed map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	if _, ok := parsed["error"]; !ok {
		t.Fatalf("error envelope missing: %s", rec.Body.String())
	}
}

// flakyUpstream 前 failFirst 个请求返回 429，之后返回成功 JSON。
type flakyUpstream struct {
	failFirst int
	seen      int
	okBody    string
}

func (f *flakyUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.seen++
	if f.seen <= f.failFirst {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(f.okBody))
}

func TestChat_FailoverToNextKey(t *testing.T) {
	up := &flakyUpstream{failFirst: 1, okBody: `{"id":"1","object":"chat.completion","model":"m"}`}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()
	client := upstream.New(upSrv.URL, 5*time.Second, 5*time.Second, 5*time.Second, "")
	// 两个 key：首选被 429，故障转移到第二个应成功。
	p := pool.New(pool.Config{SelectJitterMS: 0}, client, &staticKeys{keys: []string{"sk-a", "sk-b"}}, "")
	s := &Server{Pool: p, Client: client, Version: "test"}

	rec := do(s, http.MethodPost, "/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("failover should recover: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if up.seen < 2 {
		t.Fatalf("expected a retry on the second key, upstream saw %d requests", up.seen)
	}
}

func TestChat_FailoverExhausted(t *testing.T) {
	// 所有 key 都 429：应返回 429 而非无限重试。
	up := &flakyUpstream{failFirst: 100, okBody: `{}`}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()
	client := upstream.New(upSrv.URL, 5*time.Second, 5*time.Second, 5*time.Second, "")
	p := pool.New(pool.Config{SelectJitterMS: 0}, client, &staticKeys{keys: []string{"sk-a", "sk-b"}}, "")
	s := &Server{Pool: p, Client: client, Version: "test"}

	rec := do(s, http.MethodPost, "/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted pool should surface 429: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestChat_NonStreamRawPassthrough(t *testing.T) {
	// 上游返回的字段顺序（z 在前、a 在后）应被原样保留，不经 re-marshal 重排。
	raw := `{"z_first":1,"id":"chatcmpl-x","usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8},"a_last":2}`
	up := &fakeUpstream{contentType: "application/json", body: raw}
	s, upSrv := newTestServer(up, "")
	defer upSrv.Close()

	rec := do(s, http.MethodPost, "/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != raw {
		t.Fatalf("body not passed through verbatim:\n got: %s\nwant: %s", rec.Body.String(), raw)
	}
	// usage 应被旁路统计（不影响透传字节）。
	st := s.Pool.Snapshot()
	if len(st) != 1 || st[0].TotalIn != 3 || st[0].TotalOut != 5 {
		t.Fatalf("usage not recorded: %+v", st)
	}
}

func TestModels_MergesAlias(t *testing.T) {
	up := &fakeUpstream{
		contentType: "application/json",
		body:        `{"object":"list","data":[{"id":"kimi-latest","object":"model"}]}`,
	}
	s, upSrv := newTestServer(up, "")
	s.ModelAlias = map[string]string{"fast": "kimi-latest"}
	defer upSrv.Close()

	rec := do(s, http.MethodGet, "/v1/models", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	seen := map[string]bool{}
	for _, m := range parsed.Data {
		seen[m.ID] = true
	}
	if !seen["kimi-latest"] {
		t.Fatalf("upstream model missing: %+v", seen)
	}
	if !seen["fast"] {
		t.Fatalf("alias model missing: %+v", seen)
	}
}

func TestModelAlias_RewrittenUpstream(t *testing.T) {
	up := &fakeUpstream{contentType: "application/json", body: `{}`}
	s, upSrv := newTestServer(up, "")
	s.ModelAlias = map[string]string{"fast": "kimi-latest"}
	defer upSrv.Close()

	rec := do(s, http.MethodPost, "/v1/chat/completions", "",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(up.lastBody, `"model":"kimi-latest"`) {
		t.Fatalf("model alias not rewritten: %s", up.lastBody)
	}
}

func TestStickyKey_FromFirstUserMessage(t *testing.T) {
	req := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}]`),
	}
	k1 := stickyKey(req)
	k2 := stickyKey(req) // 历史追加后键应稳定。
	if k1 == "" {
		t.Fatal("sticky key should be derived")
	}
	if k1 != k2 {
		t.Fatalf("sticky key unstable: %q vs %q", k1, k2)
	}
	// 首条 user 消息变化应换键。
	req2 := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"different"}]`),
	}
	if stickyKey(req2) == k1 {
		t.Fatal("different conversation should produce different sticky key")
	}
}

func TestStickyKey_FromConversationID(t *testing.T) {
	// conversation_id 优先于首条 user 消息：同一对话 ID + 不同历史应得同一键。
	base := map[string]json.RawMessage{
		"conversation_id": json.RawMessage(`"conv-abc"`),
		"messages":        json.RawMessage(`[{"role":"user","content":"hello"}]`),
	}
	grow := map[string]json.RawMessage{
		"conversation_id": json.RawMessage(`"conv-abc"`),
		"messages":        json.RawMessage(`[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}]`),
	}
	other := map[string]json.RawMessage{
		"conversation_id": json.RawMessage(`"conv-xyz"`),
		"messages":        json.RawMessage(`[{"role":"user","content":"hello"}]`),
	}
	a, b, c := stickyKey(base), stickyKey(grow), stickyKey(other)
	if a == "" {
		t.Fatal("sticky key should be derived from conversation_id")
	}
	if a != b {
		t.Fatalf("same conversation_id should be stable: %q vs %q", a, b)
	}
	if a == c {
		t.Fatal("different conversation_id should produce different key")
	}
	// conversation_id 应优先于消息兜底（后者会因消息不同而变化）。
	noConv := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"different"}]`),
	}
	if a == stickyKey(noConv) {
		t.Fatal("conversation_id sticky key collided with message-derived key")
	}
}

func TestStatus_ReportsKeyState(t *testing.T) {
	up := &fakeUpstream{}
	s, upSrv := newTestServer(up, "")
	defer upSrv.Close()
	s.Pool.Disable("sk-test", "test")

	rec := do(s, http.MethodGet, "/status", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var parsed statusResp
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Total != 1 || parsed.Disabled != 1 {
		t.Fatalf("status wrong: total=%d disabled=%d", parsed.Total, parsed.Disabled)
	}
	if len(parsed.Keys) != 1 || parsed.Keys[0].State != "disabled" {
		t.Fatalf("key state wrong: %+v", parsed.Keys)
	}
	if parsed.Keys[0].Key != "***" && !strings.Contains(parsed.Keys[0].Key, "...") {
		t.Fatalf("key not masked: %q", parsed.Keys[0].Key)
	}
}

func TestHealthz(t *testing.T) {
	s, upSrv := newTestServer(&fakeUpstream{}, "")
	defer upSrv.Close()
	rec := do(s, http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}
