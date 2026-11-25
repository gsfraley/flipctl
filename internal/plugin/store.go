package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxArchived bounds the number of archived manifests kept on disk.
const MaxArchived = 200

// Store persists runtime registrations under a state directory:
//
//	<root>/ephemeral/<id>.yaml   active runtime registrations
//	<root>/installed/<id>.yaml   promoted registrations
//	<root>/archive/<owner>/<id>@<version>-<unixnano>.yaml
//
// Ephemeral manifests are archived rather than deleted when they are removed
// or cleared at startup, so a past registration can be restored later. A
// restore is a fresh registration and goes through the same validation and
// confirmation gate.
type Store struct{ Root string }

// OpenStore creates the state directory layout if needed.
func OpenStore(root string) (*Store, error) {
	for _, d := range []string{"ephemeral", "installed", "archive"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{Root: root}, nil
}

func (s *Store) tierDir(t Tier) (string, error) {
	switch t {
	case TierEphemeral, TierInstalled:
		return filepath.Join(s.Root, string(t)), nil
	}
	return "", fmt.Errorf("tier %s is not stored", t)
}

func (s *Store) path(t Tier, id string) (string, error) {
	if !safeID.MatchString(id) {
		return "", errors.New("invalid app id")
	}
	d, err := s.tierDir(t)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, id+".yaml"), nil
}

// Write atomically stores manifest for id in tier and returns its path.
func (s *Store) Write(t Tier, id string, manifest []byte) (string, error) {
	p, err := s.path(t, id)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(p, manifest); err != nil {
		return "", err
	}
	return p, nil
}

// Delete removes the stored manifest for id in tier, if present.
func (s *Store) Delete(t Tier, id string) error {
	p, err := s.path(t, id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Load parses every manifest stored in tier with the shared Parse rules.
// Files that no longer validate are reported and skipped.
func (s *Store) Load(t Tier) ([]Entry, []error) {
	d, err := s.tierDir(t)
	if err != nil {
		return nil, []error{err}
	}
	files, _ := filepath.Glob(filepath.Join(d, "*.yaml"))
	sort.Strings(files)
	var out []Entry
	var errs []error
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err == nil {
			var a App
			if a, err = Parse(b); err == nil {
				out = append(out, Entry{App: a, Tier: t, Source: f, Manifest: b})
				continue
			}
		}
		errs = append(errs, fmt.Errorf("%s: %w", f, err))
	}
	return out, errs
}

// Promote moves an ephemeral manifest into the installed tier.
func (s *Store) Promote(id string) (string, error) {
	from, err := s.path(TierEphemeral, id)
	if err != nil {
		return "", err
	}
	to, _ := s.path(TierInstalled, id)
	if err := os.Rename(from, to); err != nil {
		return "", err
	}
	return to, nil
}

// Archive keeps a copy of manifest for a later restore.
func (s *Store) Archive(a App, manifest []byte) (string, error) {
	owner := "unowned"
	if a.Owner != nil && a.Owner.Conversation != "" {
		owner = sanitize(a.Owner.Plugin + "." + a.Owner.Conversation)
	}
	d := filepath.Join(s.Root, "archive", owner)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s@%s-%d.yaml", a.ID, sanitize(firstNonEmptyString(a.Version, "0")), time.Now().UnixNano())
	p := filepath.Join(d, name)
	if err := writeAtomic(p, manifest); err != nil {
		return "", err
	}
	s.pruneArchive()
	return filepath.Join(owner, name), nil
}

// ClearEphemeral archives and deactivates every stored ephemeral manifest.
func (s *Store) ClearEphemeral() ([]string, error) {
	entries, errs := s.Load(TierEphemeral)
	var ids []string
	for _, e := range entries {
		if _, err := s.Archive(e.App, e.Manifest); err != nil {
			return ids, err
		}
		ids = append(ids, e.App.ID)
	}
	files, _ := filepath.Glob(filepath.Join(s.Root, "ephemeral", "*.yaml"))
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			return ids, err
		}
	}
	return ids, errors.Join(errs...)
}

// Archived describes one archived manifest.
type Archived struct {
	Ref      string    `json:"ref"`
	ID       string    `json:"id"`
	Version  string    `json:"version"`
	Owner    string    `json:"owner"`
	Archived time.Time `json:"archived"`
}

var archiveName = regexp.MustCompile(`^([a-zA-Z0-9_.-]+)@([a-zA-Z0-9_.-]+)-([0-9]+)\.yaml$`)

// ListArchive returns archived manifests, newest first.
func (s *Store) ListArchive() []Archived {
	files, _ := filepath.Glob(filepath.Join(s.Root, "archive", "*", "*.yaml"))
	var out []Archived
	for _, f := range files {
		m := archiveName.FindStringSubmatch(filepath.Base(f))
		if m == nil {
			continue
		}
		ns, _ := strconv.ParseInt(m[3], 10, 64)
		owner := filepath.Base(filepath.Dir(f))
		out = append(out, Archived{Ref: owner + "/" + filepath.Base(f), ID: m[1], Version: m[2], Owner: owner, Archived: time.Unix(0, ns).UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Archived.After(out[j].Archived) })
	return out
}

// ReadArchive returns the manifest for ref, or for the newest archived
// version of id when ref is empty.
func (s *Store) ReadArchive(id, ref string) ([]byte, error) {
	for _, a := range s.ListArchive() {
		if a.ID == id && (ref == "" || a.Ref == ref) {
			return os.ReadFile(filepath.Join(s.Root, "archive", filepath.FromSlash(a.Ref)))
		}
	}
	return nil, ErrNotFound
}

func (s *Store) pruneArchive() {
	all := s.ListArchive()
	for _, a := range all[min(len(all), MaxArchived):] {
		_ = os.Remove(filepath.Join(s.Root, "archive", filepath.FromSlash(a.Ref)))
	}
}

func writeAtomic(p string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

func sanitize(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	return strings.Trim(s, ".")
}

func firstNonEmptyString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
