package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeChannel writes a minimal Roku channel source tree and returns its dir.
func fakeChannel(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("manifest", "title=Test\n")
	write("source/Main.brs", "sub Main()\nend sub\n")
	write("components/Scene.xml", "<component name=\"Scene\" />")
	// The on-device crypto self-check reads its golden vectors from
	// pkg:/testdata, so a packaged channel must carry it — see channelRoots.
	write("testdata/golden.json", "{}")
	write("components/.DS_Store", "junk")
	write(".git/config", "junk")
	write("README.md", "not channel content")
	return dir
}

func TestBuildChannelZipPackagesOnlyChannelContent(t *testing.T) {
	data, count, err := buildChannelZip(fakeChannel(t))
	if err != nil {
		t.Fatalf("buildChannelZip: %v", err)
	}
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read built zip: %v", err)
	}

	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	want := []string{"components/Scene.xml", "manifest", "source/Main.brs", "testdata/golden.json"}
	if len(names) != len(want) {
		t.Fatalf("zip holds %v, want exactly %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("entry %d = %q, want %q (sorted order)", i, names[i], want[i])
		}
	}
	if count != len(want) {
		t.Errorf("reported %d file(s), want %d", count, len(want))
	}
	// README.md is outside channelRoots and .DS_Store/.git are dotted: neither
	// belongs in a channel, and the installer's compile step reports a
	// confusing error rather than ignoring them.
	for _, n := range names {
		if strings.Contains(n, "DS_Store") || strings.Contains(n, ".git") || strings.HasSuffix(n, ".md") {
			t.Errorf("non-channel file %q was packaged", n)
		}
	}
}

// Two builds of identical source must produce identical bytes, so "is the wall
// running the build I think it is?" is answerable by comparing digests.
func TestBuildChannelZipIsDeterministic(t *testing.T) {
	dir := fakeChannel(t)
	first, _, err := buildChannelZip(dir)
	if err != nil {
		t.Fatalf("buildChannelZip: %v", err)
	}
	second, _, err := buildChannelZip(dir)
	if err != nil {
		t.Fatalf("buildChannelZip: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two builds of the same source produced different bytes; a real mtime leaked into the archive")
	}
}

func TestBuildChannelZipRequiresManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "source"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, _, err := buildChannelZip(dir); err == nil {
		t.Fatal("a source tree with no manifest packaged successfully")
	}
}

// The real player-v3 tree must package — this is the build the fleet actually
// receives, so a change that moves a source root breaks here rather than on a
// wall of TVs.
func TestBuildChannelZipPackagesTheRealPlayer(t *testing.T) {
	data, count, err := buildChannelZip(filepath.Join("..", "..", "player-v3"))
	if err != nil {
		t.Fatalf("packaging the real player-v3 tree: %v", err)
	}
	if count < 5 {
		t.Errorf("packaged only %d file(s) from player-v3; the tree has a manifest, sources and components", count)
	}
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read built zip: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range r.File {
		seen[f.Name] = true
	}
	for _, required := range []string{
		"manifest",
		"components/PlayerTask.brs",
		"components/PlayerTask.xml",
		// The self-check's golden vectors: absent from bsconfig's `files`,
		// required at runtime (channelRoots' own note).
		"testdata/golden-relay-cert.pem",
	} {
		if !seen[required] {
			t.Errorf("the packaged channel is missing %q", required)
		}
	}
}

func TestLoadChannelZipRejectsFolderWrappedArchive(t *testing.T) {
	// What every desktop "Compress" action produces: the channel nested one
	// level down. The firmware accepts the upload and then fails to compile,
	// once per device, after one full upload each.
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create("player-v3/manifest")
	if err != nil {
		t.Fatalf("create entry: %v", err)
	}
	if _, err := f.Write([]byte("title=Test\n")); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	path := filepath.Join(t.TempDir(), "wrapped.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}
	if _, _, err := loadChannelZip(path); err == nil {
		t.Fatal("a folder-wrapped archive was accepted as a channel zip")
	}
}

func TestLoadChannelZipAcceptsABuiltChannel(t *testing.T) {
	data, _, err := buildChannelZip(fakeChannel(t))
	if err != nil {
		t.Fatalf("buildChannelZip: %v", err)
	}
	path := filepath.Join(t.TempDir(), "channel.zip")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}
	loaded, count, err := loadChannelZip(path)
	if err != nil {
		t.Fatalf("loadChannelZip rejected this tool's own output: %v", err)
	}
	if count != 4 || !bytes.Equal(loaded, data) {
		t.Errorf("loaded %d file(s) / %d bytes, want the 4 entries and the exact bytes back", count, len(loaded))
	}
}
