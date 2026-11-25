package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreClearEphemeralArchivesForRestore(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := strings.Replace(pingManifest, "version: 0.1.0\n", "version: 0.1.0\nowner: {plugin: assistant, conversation: c-12}\n", 1)
	if _, err := Parse([]byte(m)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(TierEphemeral, "network.example", []byte(m)); err != nil {
		t.Fatal(err)
	}
	ids, err := s.ClearEphemeral()
	if err != nil || len(ids) != 1 || ids[0] != "network.example" {
		t.Fatalf("clear: %v %v", ids, err)
	}
	if left, _ := s.Load(TierEphemeral); len(left) != 0 {
		t.Fatalf("ephemeral survived clear: %v", left)
	}
	arch := s.ListArchive()
	if len(arch) != 1 || arch[0].Owner != "assistant.c-12" || arch[0].Version != "0.1.0" {
		t.Fatalf("archive: %#v", arch)
	}
	b, err := s.ReadArchive("network.example", "")
	if err != nil || string(b) != m {
		t.Fatalf("restore read: %v", err)
	}
	if _, err := s.ReadArchive("network.example", "../../etc/passwd"); err == nil {
		t.Fatal("arbitrary archive ref accepted")
	}
}

func TestStorePromoteMovesManifestToInstalled(t *testing.T) {
	root := t.TempDir()
	s, _ := OpenStore(root)
	if _, err := s.Write(TierEphemeral, "network.example", []byte(pingManifest)); err != nil {
		t.Fatal(err)
	}
	p, err := s.Promote("network.example")
	if err != nil || p != filepath.Join(root, "installed", "network.example.yaml") {
		t.Fatalf("promote: %s %v", p, err)
	}
	installed, errs := s.Load(TierInstalled)
	if len(errs) != 0 || len(installed) != 1 || installed[0].Tier != TierInstalled {
		t.Fatalf("installed: %#v %v", installed, errs)
	}
	if _, err := os.Stat(filepath.Join(root, "ephemeral", "network.example.yaml")); !os.IsNotExist(err) {
		t.Fatal("ephemeral copy remains after promote")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode %v", fi.Mode().Perm())
	}
}

func TestStoreRejectsUnsafeIDs(t *testing.T) {
	s, _ := OpenStore(t.TempDir())
	if _, err := s.Write(TierEphemeral, "../escape", []byte("x")); err == nil {
		t.Fatal("path traversal id accepted")
	}
	if _, err := s.Write(TierSystem, "x", []byte("x")); err == nil {
		t.Fatal("system tier written to state dir")
	}
}
