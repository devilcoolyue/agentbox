package agent

import (
	"bytes"
	"strings"
	"testing"
)

func TestModelProbeCatalogAndRelayIsolation(t *testing.T) {
	for _, provider := range []string{"openai", "relay"} {
		script := `{"id":1,"result":{}}
{"id":2,"result":{"config":{"model_provider":"` + provider + `"}}}
{"id":3,"result":{"data":[{"model":"fixture","supportedReasoningEfforts":[{"reasoningEffort":"high"},{"reasoningEffort":"max"}]}],"nextCursor":"next"}}
{"id":4,"result":{"data":[{"model":"fixture-2","supportedReasoningEfforts":[{"reasoningEffort":"low"}]}],"nextCursor":null}}
`
		var sent bytes.Buffer
		models, err := ProbeCodexModels(&sent, strings.NewReader(script))
		if err != nil {
			t.Fatal(err)
		}
		if provider == "relay" {
			if len(models) != 0 || strings.Contains(sent.String(), "model/list") {
				t.Fatal("applied official catalog to relay")
			}
		} else if len(models) != 2 || len(models["fixture"].Levels) != 2 {
			t.Fatalf("lost catalog data: %+v", models)
		}
		if strings.Contains(sent.String(), "thread/start") || strings.Contains(sent.String(), "turn/start") {
			t.Fatal("metadata probe started inference")
		}
	}
}
