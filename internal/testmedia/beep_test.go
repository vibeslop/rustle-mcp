package testmedia

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// The hash is of the file test/wav.ts's writeBeep writes.
func TestWriteBeepMatchesTheTypeScriptHelper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beep.wav")
	if err := WriteBeep(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != "1cd952cfa7ca388b9793b43b601fff8074079b7165e4b352173592fc4ee3421e" {
		t.Fatalf("sha256 %s", got)
	}
}
