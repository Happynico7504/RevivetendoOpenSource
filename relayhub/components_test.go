package relayhub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/Happynico7504/relaylink"
)

func TestComponentReleasesAreIndependent(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	if _, err := PublishRelease(dir, []byte("relayd-bin-7"), 7, "", "linux", "amd64", priv, false); err != nil {
		t.Fatal(err)
	}
	// The edge starts its own version line at 1, far below relayd's 7.
	m, err := PublishComponent(dir, "wscedge", []byte("edge-bin-1"), 1, "first", "linux", "amd64", priv, false)
	if err != nil || m.Component != "wscedge" {
		t.Fatalf("publish edge: %+v %v", m, err)
	}
	s := &ReleaseStore{Dir: dir}

	rd, err := s.Manifest("linux", "amd64")
	if err != nil || rd.Version != 7 || rd.Component != "" {
		t.Fatalf("relayd manifest: %+v %v (must stay component-less)", rd, err)
	}
	ed, err := s.ManifestFor("wscedge", "linux", "amd64")
	if err != nil || ed.Version != 1 || ed.ComponentName() != "wscedge" {
		t.Fatalf("edge manifest: %+v %v", ed, err)
	}
	if c, err := s.ChunkFor("wscedge", "linux", "amd64", 1, 0); err != nil || string(c.Data) != "edge-bin-1" || !c.EOF {
		t.Fatalf("edge chunk: %+v %v", c, err)
	}
	if c, err := s.Chunk("linux", "amd64", 7, 0); err != nil || string(c.Data) != "relayd-bin-7" {
		t.Fatalf("relayd chunk: %+v %v", c, err)
	}
	// A version number of one component is meaningless to another.
	if _, err := s.ChunkFor("wscedge", "linux", "amd64", 7, 0); err == nil {
		t.Fatal("served the edge binary for relayd's version number")
	}
	// Publishing the edge again at the same version is refused, without touching relayd.
	if _, err := PublishComponent(dir, "wscedge", []byte("x"), 1, "", "linux", "amd64", priv, false); err == nil {
		t.Fatal("same edge version published twice")
	}
	if got := s.Components(); len(got) != 2 || got[0] != "relayd" || got[1] != "wscedge" {
		t.Fatalf("components: %v", got)
	}
}

func TestComponentNamesCannotEscapeOrCollide(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, bad := range []string{"../etc", "a/b", "linux-amd64", "Edge", "", ".", "1edge", "wsc-edge"} {
		if bad == "" {
			continue // empty means relayd
		}
		if _, err := PublishComponent(dir, bad, []byte("x"), 1, "", "linux", "amd64", priv, false); err == nil {
			t.Errorf("published component %q", bad)
		}
		if _, err := (&ReleaseStore{Dir: dir}).ManifestFor(bad, "linux", "amd64"); err == nil {
			t.Errorf("served component %q", bad)
		}
	}
	// Nothing was created outside the releases directory by those attempts.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("stray files: %v", ents)
	}
}

// A manifest filed under the wrong component is never served, even if its signature
// and binary are fine (someone copying a release into the wrong folder).
func TestMisfiledComponentIsNotServed(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := PublishComponent(dir, "wscedge", []byte("edge"), 1, "", "linux", "amd64", priv, false); err != nil {
		t.Fatal(err)
	}
	// Move the edge release into another component's folder.
	if err := os.MkdirAll(filepath.Join(dir, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "wscedge", "linux-amd64"), filepath.Join(dir, "other", "linux-amd64")); err != nil {
		t.Fatal(err)
	}
	os.Rename(filepath.Join(dir, "other", "linux-amd64", "wscedge"), filepath.Join(dir, "other", "linux-amd64", "other"))
	if _, err := (&ReleaseStore{Dir: dir}).ManifestFor("other", "linux", "amd64"); err == nil {
		t.Fatal("served a wscedge manifest as component 'other'")
	}
}

func TestHubRoutesComponentQuery(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	PublishRelease(dir, []byte("relayd-bin"), 2, "", "linux", "amd64", priv, false)
	PublishComponent(dir, "wscedge", []byte("edge-bin"), 1, "", "linux", "amd64", priv, false)
	h := &Hub{Log: NewInvalidationLog(10), Rel: &ReleaseStore{Dir: dir}}
	get := func(path string) *relaylink.Response {
		return h.Dispatch(context.Background(), &relaylink.Request{Method: "GET", Path: path})
	}
	if r := get(relaylink.UpdateManifestPath + "?os=linux&arch=amd64"); r.Status != 200 {
		t.Fatalf("relayd (no component param, as old relays ask): %d", r.Status)
	}
	if r := get(relaylink.UpdateManifestPath + "?component=wscedge&os=linux&arch=amd64"); r.Status != 200 {
		t.Fatalf("edge manifest: %d", r.Status)
	}
	if r := get(relaylink.UpdateChunkPath + "?component=wscedge&os=linux&arch=amd64&version=1&offset=0"); r.Status != 200 {
		t.Fatalf("edge chunk: %d", r.Status)
	}
	if r := get(relaylink.UpdateManifestPath + "?component=nosuch&os=linux&arch=amd64"); r.Status != 404 {
		t.Fatalf("unknown component: %d", r.Status)
	}
	if r := get(relaylink.UpdateManifestPath + "?component=..%2Fetc&os=linux&arch=amd64"); r.Status != 404 {
		t.Fatalf("traversal in component: %d", r.Status)
	}
}
