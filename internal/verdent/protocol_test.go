package verdent

import (
	"encoding/json"
	"strings"
	"testing"

	"verdent2api/internal/crypto"
)

func TestBuildBody_EncryptsMessagesAndKeepsSystem(t *testing.T) {
	c := NewClient("http://127.0.0.1", 0, 0, 0)
	body, err := c.BuildBody(BuildParams{
		Model: "deepseek-v4.1-flash-free",
		Messages: []OAIMessage{
			{Role: "system", Content: json.RawMessage(`"be brief"`)},
			{Role: "user", Content: json.RawMessage(`"你好"`)},
		},
		Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var model string
	if json.Unmarshal(body["model"], &model) != nil || model != "deepseek-v4.1-flash-free" {
		t.Fatalf("model=%s", body["model"])
	}
	var sys string
	if json.Unmarshal(body["system"], &sys) != nil || sys == "" {
		t.Fatal("template system was overwritten or dropped")
	}
	var blob string
	if json.Unmarshal(body["messages"], &blob) != nil {
		t.Fatal("messages missing")
	}
	var msgs []map[string]interface{}
	if err := crypto.DecryptBlob(blob, &msgs); err != nil {
		t.Fatal(err)
	}
	foundSys, foundHi := false, false
	for _, m := range msgs {
		raw, _ := json.Marshal(m)
		if strings.Contains(string(raw), "be brief") {
			foundSys = true
		}
		if strings.Contains(string(raw), "你好") {
			foundHi = true
		}
	}
	if !foundSys || !foundHi {
		t.Fatalf("system not folded into messages: %+v", msgs)
	}
}

func TestTranslate_StreamAndAggregate(t *testing.T) {
	sse := "" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"你\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"好\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2},\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	tr := NewTranslator(strings.NewReader(sse), "deepseek-v4.1-flash-free")
	var frames []string
	usage, err := tr.StreamTo(func(b []byte) { frames = append(frames, string(b)) })
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, `"content":"你"`) || !strings.Contains(joined, "data: [DONE]") {
		t.Fatalf("stream frames incomplete: %s", joined)
	}
	if usage.In != 3 || usage.Out != 2 {
		t.Fatalf("usage=%+v", usage)
	}

	tr2 := NewTranslator(strings.NewReader(sse), "deepseek-v4.1-flash-free")
	obj, u2, err := tr2.Aggregate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(obj), `"content":"你好"`) || u2.Out != 2 {
		t.Fatalf("aggregate=%s usage=%+v", obj, u2)
	}
}

func TestCatalog_DefaultFreeModels(t *testing.T) {
	c := NewCatalog("/no/such/file")
	free := c.FreeModels()
	if len(free) < 2 {
		t.Fatalf("expected embedded free models, got %+v", free)
	}
	seen := map[string]bool{}
	for _, m := range free {
		seen[m.Key] = true
		if !m.IsFree() {
			t.Fatalf("%s not marked free", m.Key)
		}
	}
	if !seen["deepseek-v4.1-flash-free"] || !seen["glm-5.3-flash-free"] {
		t.Fatalf("missing known free models: %+v", seen)
	}
}
