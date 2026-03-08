package events

import "testing"

func TestSchemaVersionNotEmpty(t *testing.T) {
	if SchemaVersion == "" {
		t.Fatal("schema version must not be empty")
	}
}
