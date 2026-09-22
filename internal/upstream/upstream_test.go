package upstream

import (
	"io"
	"strings"
	"testing"
)

func TestSSEReader_Next(t *testing.T) {
	raw := ": ping\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"\n" +
		"data: [DONE]\n\n"
	r := NewSSEReader(strings.NewReader(raw))

	first, err := r.Next()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if !strings.Contains(string(first), `"content":"hello"`) {
		t.Fatalf("unexpected payload: %s", first)
	}

	// 第二次应命中 [DONE]。
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("expected EOF at [DONE], got %v", err)
	}
}

func TestParseChunk_Normalize(t *testing.T) {
	in := `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`
	c, err := ParseChunk([]byte(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Choices[0].Delta.Content != "hi" {
		t.Fatalf("bad content: %q", c.Choices[0].Delta.Content)
	}
	if c.Choices[0].FinishReason != nil {
		t.Fatalf("finish_reason should be null pointer, got %v", *c.Choices[0].FinishReason)
	}
}

func TestNormalize_FillsDefaults(t *testing.T) {
	// 上游缺字段时补齐标准 OpenAI 结构。
	c := &Chunk{Choices: []Choice{{Index: 0, Delta: Delta{Content: "x"}}}}
	out := Normalize(c, "model-x", "chatcmpl-1", 42)
	if !strings.Contains(string(out), `"object":"chat.completion.chunk"`) {
		t.Fatalf("missing object: %s", out)
	}
	if !strings.Contains(string(out), `"model":"model-x"`) {
		t.Fatalf("missing model: %s", out)
	}
	if !strings.Contains(string(out), `"id":"chatcmpl-1"`) {
		t.Fatalf("missing id: %s", out)
	}
}

func TestNormalize_ReasoningFieldUnification(t *testing.T) {
	// DeepSeek 系思维链字段统一到 reasoning_content。
	c := &Chunk{Choices: []Choice{{Index: 0, Delta: Delta{Reasoning: "think"}}}}
	out := Normalize(c, "m", "id", 1)
	if !strings.Contains(string(out), `"reasoning_content":"think"`) {
		t.Fatalf("reasoning not unified: %s", out)
	}
	if strings.Contains(string(out), `"reasoning":"think"`) {
		t.Fatalf("raw reasoning field should be cleared: %s", out)
	}
}

func TestNormalize_MessageToDelta(t *testing.T) {
	// 部分上游把首帧放在 message 而非 delta。
	msg := "assistant"
	c := &Chunk{Choices: []Choice{{Index: 0, Message: &Delta{Role: msg}, FinishReason: nil}}}
	out := Normalize(c, "m", "id", 1)
	if !strings.Contains(string(out), `"role":"assistant"`) {
		t.Fatalf("message not promoted to delta: %s", out)
	}
	if strings.Contains(string(out), `"message"`) {
		t.Fatalf("message field should be dropped: %s", out)
	}
}

func TestFrame(t *testing.T) {
	got := string(Frame([]byte(`{"a":1}`)))
	if got != "data: {\"a\":1}\n\n" {
		t.Fatalf("bad frame: %q", got)
	}
	if string(DoneFrame()) != "data: [DONE]\n\n" {
		t.Fatalf("bad done frame")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   Class
	}{
		{401, `{"error":{"message":"authentication required"}}`, ClassKeyDead},
		{403, `forbidden`, ClassKeyDead},
		{401, `insufficient credits`, ClassNoCredit},
		{402, `quota exceeded`, ClassNoCredit},
		{429, `rate limit`, ClassRateLimited},
		{429, `insufficient credits`, ClassNoCredit},
		{500, `internal`, ClassTransient},
		{503, `unavailable`, ClassTransient},
		{400, `bad request`, ClassOther},
	}
	for _, c := range cases {
		if got := classify(c.status, c.body); got != c.want {
			t.Errorf("classify(%d,%q)=%s want %s", c.status, c.body, got, c.want)
		}
	}
}

func TestExtractOpenAIErrorMessage(t *testing.T) {
	cases := map[string]string{
		`{"type":"error","error":{"type":"authentication_error","message":"invalid credential"}}`: "invalid credential",
		`{"error":{"message":"quota exceeded","type":"billing"}}`:                                     "quota exceeded",
		`{"message":"plain message"}`: "plain message",
		`not json`:                    "",
	}
	for in, want := range cases {
		if got := extractOpenAIErrorMessage([]byte(in)); got != want {
			t.Errorf("extract(%s)=%q want %q", in, got, want)
		}
	}
}
