package main

import (
	"path/filepath"
	"testing"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
)

func TestOperatorActionCLICompositionUsesOnlyDurableStore(t *testing.T) {
	store, err := sqlitestore.Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if service, err := composeOperatorActionService(store); err != nil || service == nil {
		t.Fatalf("service=%v err=%v", service, err)
	}
	if service, err := composeOperatorActionService(nil); err == nil || service != nil {
		t.Fatalf("nil service=%v err=%v", service, err)
	}
}
