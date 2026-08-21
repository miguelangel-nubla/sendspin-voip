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

// TestFileStore_NullStateFileDoesNotPanic covers a state file holding JSON
// "null" — which is what a crashed or truncated write can leave behind, and
// what MarshalIndent emits for a nil map. It unmarshals into a nil map, and
// assigning that over the initialised map made the next write panic with
// "assignment to entry in nil map".
func TestFileStore_NullStateFileDoesNotPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("null"), 0o644); err != nil {
		t.Fatalf("failed to seed state file: %v", err)
	}

	store := NewFileStore(path)

	if err := store.SetPlayerState("player-desk", app.PlayerStateRecord{Volume: 42, Muted: true}); err != nil {
		t.Fatalf("SetPlayerState failed: %v", err)
	}

	rec, ok := store.GetPlayerState("player-desk")
	if !ok || rec.Volume != 42 || !rec.Muted {
		t.Errorf("expected persisted {42 true}, got %+v (ok=%v)", rec, ok)
	}

	// And it must survive a round trip through disk.
	reloaded := NewFileStore(path)
	if rec, ok := reloaded.GetPlayerState("player-desk"); !ok || rec.Volume != 42 {
		t.Errorf("expected reloaded volume 42, got %+v (ok=%v)", rec, ok)
	}
}
