package tooling

import (
	"encoding/json"
	"testing"

	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

func TestWriteStdinSchemaExposesNumericSessionID(t *testing.T) {
	info, err := toolutils.GoStruct2ToolInfo[WriteStdinInput](
		ToolNameWriteStdin,
		"Write to a running process.",
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		JSONSchema struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
			Required []string `json:"required"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if got := document.JSONSchema.Properties["session_id"].Type; got != "integer" {
		t.Fatalf("session_id schema type = %q, want integer; schema: %s", got, encoded)
	}
	required := false
	for _, name := range document.JSONSchema.Required {
		if name == "session_id" {
			required = true
			break
		}
	}
	if !required {
		t.Fatalf("session_id is not required; schema: %s", encoded)
	}
}
