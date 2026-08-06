package moonshot

import (
	"encoding/json"
	"testing"
)

// TestMessageContentKeyAlwaysPresent guards against reintroducing
// `omitempty` on Content: a tool-role message with an empty result must
// still send `"content":""`, which upstream requires.
func TestMessageContentKeyAlwaysPresent(t *testing.T) {
	msg := Message{Role: "tool", Content: "", ToolCallID: "call_1"}

	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["content"]; !ok {
		t.Errorf("marshaled message %s is missing the required \"content\" key", out)
	}
}
