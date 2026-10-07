package protocol

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"agentbox/internal/store"
)

// Check real Go wire fields against the same schema that generates TS types.
// This deliberately covers just the new protocols, not the whole HTTP API.
func TestWireSchemaV1(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/chat-errors-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]map[string]any `json:"properties"`
			Required   []string                  `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	models := map[string]any{
		"ChatRequestInput": store.ChatRequestInput{}, "ChatRequestReceipt": store.ChatRequest{},
		"ChatRequestResponse": ChatRequestResponse{}, "ChatRequestList": ChatRequestList{},
		"ChatRequestEvent": ChatRequestEvent{}, "APIProblem": APIProblem{},
	}
	for name, model := range models {
		t.Run(name, func(t *testing.T) {
			fields := map[string]reflect.StructField{}
			var visit func(reflect.Type)
			visit = func(typ reflect.Type) {
				for i := 0; i < typ.NumField(); i++ {
					f := typ.Field(i)
					if f.Anonymous {
						visit(f.Type)
						continue
					}
					key := strings.Split(f.Tag.Get("json"), ",")[0]
					if key != "-" {
						fields[key] = f
					}
				}
			}
			visit(reflect.TypeOf(model))
			def, ok := schema.Defs[name]
			if !ok || len(fields) != len(def.Properties) {
				t.Fatal("schema field set differs from Go")
			}
			for key, prop := range def.Properties {
				f, ok := fields[key]
				if !ok {
					t.Fatalf("missing Go field %s", key)
				}
				typ := f.Type
				if typ.Kind() == reflect.Pointer {
					typ = typ.Elem()
				}
				kind := map[reflect.Kind]string{reflect.String: "string", reflect.Int: "integer", reflect.Int64: "integer", reflect.Bool: "boolean", reflect.Slice: "array", reflect.Struct: "object"}[typ.Kind()]
				if want, ok := prop["type"].(string); ok && kind != want {
					t.Fatalf("%s type %s != %s", key, kind, want)
				}
				if ref, ok := prop["$ref"].(string); ok && reflect.TypeOf(models[strings.TrimPrefix(ref, "#/$defs/")]) != typ {
					t.Fatalf("%s reference differs", key)
				}
				if name != "APIProblem" {
					required := false
					for _, r := range def.Required {
						if r == key {
							required = true
						}
					}
					if required == strings.Contains(f.Tag.Get("json"), ",omitempty") {
						t.Fatalf("%s required/omitempty differs", key)
					}
				}
			}
		})
	}
	if len(models) != len(schema.Defs) {
		t.Fatal("untested schema definition")
	}
	var examples struct {
		Version  int
		Receipts []store.ChatRequest
	}
	raw, err = os.ReadFile("../../contracts/chat-v1.examples.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &examples); err != nil {
		t.Fatal(err)
	}
	want := []string{store.ChatAccepted, store.ChatStarting, store.ChatRunning, store.ChatCompleted, store.ChatFailed, store.ChatInterrupted, store.ChatUncertain, store.ChatReviewed, store.ChatAbandoned, store.ChatDeleted}
	states := schema.Defs["ChatRequestReceipt"].Properties["state"]["enum"].([]any)
	if examples.Version != ChatVersion || len(want) != len(states) || len(want) != len(examples.Receipts) {
		t.Fatal("version or state coverage differs")
	}
	for i, state := range want {
		if state != states[i] || examples.Receipts[i].State != state {
			t.Fatal("state contract differs", state)
		}
		c := examples.Receipts[i]
		raw, err := json.Marshal(Response(c, false))
		if err != nil {
			t.Fatal(err)
		}
		var reply map[string]json.RawMessage
		_ = json.Unmarshal(raw, &reply)
		if string(reply["version"]) != "1" || string(reply["replayed"]) != "false" {
			t.Fatal("false replay flag/version lost")
		}
		if Event(c).Type != "chat_request" || Event(c).Version != ChatVersion {
			t.Fatal("event discriminator differs")
		}
	}
}
