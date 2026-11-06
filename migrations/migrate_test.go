package migrations

import "testing"

func TestMigrationNames(t *testing.T) {
	if err := ValidateVersionNames(); err != nil {
		t.Fatal(err)
	}
}
