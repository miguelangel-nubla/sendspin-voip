package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
)

func TestFileStore_SaveAndLoad(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "statestore-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	filePath := filepath.Join(tmpDir, "state.json")
	store := NewFileStore(filePath)

	if _, ok := store.GetPlayerState("player-1"); ok {
		t.Errorf("expected player-1 not to exist")
	}

	if err := store.SetPlayerState("player-1", app.PlayerStateRecord{Volume: 58, Muted: false}); err != nil {
		t.Fatalf("failed to set state: %v", err)
	}

	rec, ok := store.GetPlayerState("player-1")
	if !ok || rec.Volume != 58 || rec.Muted {
		t.Errorf("unexpected record: %+v", rec)
	}

	store2 := NewFileStore(filePath)
	rec2, ok2 := store2.GetPlayerState("player-1")
	if !ok2 || rec2.Volume != 58 || rec2.Muted {
		t.Errorf("unexpected reloaded record: %+v", rec2)
	}
}
