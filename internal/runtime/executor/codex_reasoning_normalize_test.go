package executor

import (
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestNormalizeCodexReasoningEffortForModelClampsMaxToXHigh(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"max"},"input":"hi"}`)
	out := normalizeCodexReasoningEffortForModel(body, cliproxyexecutor.Request{Model: "gpt-5.5", Payload: body}, "gpt-5.5")
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh; body=%s", got, out)
	}
}

func TestNormalizeCodexReasoningEffortForModelKeepsSupportedMax(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"max"},"input":"hi"}`)
	out := normalizeCodexReasoningEffortForModel(body, cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: body}, "gpt-6-astra")
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "max" {
		t.Fatalf("reasoning.effort = %q, want max; body=%s", got, out)
	}
}
