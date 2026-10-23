package schemas

import (
	"encoding/json"
	"testing"
)

func TestConfigURLIsTheSchemaID(t *testing.T) {
	var doc struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(Config(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ID != ConfigURL {
		t.Errorf("the schema's $id is %q, ConfigURL is %q", doc.ID, ConfigURL)
	}
}
