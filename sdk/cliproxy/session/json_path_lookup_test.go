package session

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestFirstNormalizedPathValuePrefersListedOrder(t *testing.T) {
	root := gjson.Parse(`{"metadata":{"parent_thread_id":"thread-parent"},"parent_id":"top-parent"}`)
	if got := firstNormalizedPathValue(root); got != "top-parent" {
		t.Fatalf("firstNormalizedPathValue() = %q, want top-parent", got)
	}
	nested := gjson.Parse(`{"request":{"metadata":{"parent_thread_id":"nested-parent"}},"contents":[]}`)
	req := nested.Get("request")
	if got := firstNormalizedPathValue(nested, req); got != "nested-parent" {
		t.Fatalf("firstNormalizedPathValue(nested) = %q, want nested-parent", got)
	}
}

func BenchmarkFirstNormalizedPathValueLargeBody(b *testing.B) {
	var body strings.Builder
	body.WriteString(`{"model":"gpt-5.6-sol","instructions":"`)
	body.WriteString(strings.Repeat("x", 20000))
	body.WriteString(`","input":[`)
	for i := 0; i < 400; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"`)
		body.WriteString(strings.Repeat("y", 1200))
		body.WriteString(`"}]}`)
	}
	body.WriteString(`],"metadata":{"parent_thread_id":"019b-parent"}}`)
	root := gjson.Parse(body.String())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := firstNormalizedPathValue(root); got != "019b-parent" {
			b.Fatalf("got %q", got)
		}
	}
}
