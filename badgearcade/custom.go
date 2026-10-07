package badgearcade

// Adding custom badges and crane machines to a weekly crane archive
// (sharc/<week>.sarc inside data_v131.dat). Every file must also be listed in
// pc/PrizeCollection.xml, the archive's registry of all prizes and machines.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PrizePath and CraneInstancePath are where the game looks for the files.
func PrizePath(name string) string         { return "pc/rt/Pr/" + name + ".prb.szs" }
func CraneInstancePath(name string) string { return "pc/ci/" + name + ".cib.szs" }

const prizeCollectionPath = "pc/PrizeCollection.xml"

// ArchiveFile is a file to add to a weekly crane archive. Kind is its
// PrizeCollection.xml list ("Prize", "CraneInstance", "Crane", "CraneIcon",
// "FixedObject", "Attachment", "Category"), so it gets registered there under
// Name.
type ArchiveFile struct {
	Path string
	Data []byte
	Kind string
	Name string
}

// RegisterInPrizeCollection adds names to the given PrizeCollection.xml lists
// (keyed by kind, e.g. "Prize" -> <Prizes>), skipping ones already listed and
// updating the lists' count attributes. Nintendo's file is UTF-8 with a BOM
// and CRLF line endings.
func RegisterInPrizeCollection(xml string, names map[string][]string) (string, error) {
	kinds := make([]string, 0, len(names))
	for k := range names {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		list := kind + "s"
		if kind == "Category" {
			list = "Categories"
		}
		closing := "</" + list + ">"
		i := strings.Index(xml, closing)
		if i < 0 {
			return "", fmt.Errorf("badgearcade: PrizeCollection.xml has no %s", closing)
		}
		lineStart := strings.LastIndex(xml[:i], "\n") + 1 // keep the closing tag's indentation
		added := 0
		var b strings.Builder
		for _, n := range names[kind] {
			entry := fmt.Sprintf(`<%s name="%s" />`, kind, n)
			if strings.Contains(xml, entry) || strings.Contains(b.String(), entry) {
				continue
			}
			b.WriteString("    " + entry + "\r\n")
			added++
		}
		xml = xml[:lineStart] + b.String() + xml[lineStart:]
		countRe := regexp.MustCompile(`<` + list + ` count="(\d+)">`)
		if m := countRe.FindStringSubmatch(xml); m != nil {
			n, _ := strconv.Atoi(m[1])
			xml = strings.Replace(xml, m[0], fmt.Sprintf(`<%s count="%d">`, list, n+added), 1)
		}
	}
	return xml, nil
}

// AddCustomContent adds badges, crane machines and any other files (e.g. a
// machine's stage and objects copied from another region) to a weekly crane
// archive and registers them, returning the rebuilt archive. Files with the
// same path are replaced.
func AddCustomContent(weekly []byte, prizes []*Prize, instances []*CraneInstance, extra ...ArchiveFile) ([]byte, error) {
	entries, err := ParseSARC(weekly)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	names := map[string][]string{}
	for _, p := range prizes {
		files[PrizePath(p.Name())] = CompressYaz0(p.Bytes())
		names["Prize"] = append(names["Prize"], p.Name())
	}
	for _, c := range instances {
		raw, err := c.Bytes()
		if err != nil {
			return nil, err
		}
		files[CraneInstancePath(c.Name)] = CompressYaz0(raw)
		names["CraneInstance"] = append(names["CraneInstance"], c.Name)
	}
	for _, f := range extra {
		files[f.Path] = f.Data
		if f.Kind != "" {
			names[f.Kind] = append(names[f.Kind], f.Name)
		}
	}
	found := false
	out := make([]SARCEntry, 0, len(entries)+len(files))
	for _, e := range entries {
		if _, replaced := files[e.Name]; replaced {
			continue
		}
		if e.Name == prizeCollectionPath {
			xml, err := RegisterInPrizeCollection(string(e.Data), names)
			if err != nil {
				return nil, err
			}
			e = SARCEntry{Name: e.Name, Data: []byte(xml)}
			found = true
		}
		out = append(out, e)
	}
	if !found {
		return nil, fmt.Errorf("badgearcade: archive has no %s", prizeCollectionPath)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths) // BuildSARC sorts by hash; this just keeps runs deterministic
	for _, p := range paths {
		out = append(out, SARCEntry{Name: p, Data: files[p]})
	}
	// Nintendo's weekly archives use 128-byte alignment (rebuilds byte-exact).
	return BuildSARC(out, 128, 128), nil
}

// MachinePartPaths lists the files a machine needs besides its badges: the
// stage (.crb), the crane icon (.icb), fixed objects (.fob) and attachments
// (.atb), each with its PrizeCollection.xml kind and name.
func MachinePartPaths(c *CraneInstance) []ArchiveFile {
	parts := []ArchiveFile{
		{Path: "pc/rt/Cr/" + c.Crane + ".crb.szs", Kind: "Crane", Name: c.Crane},
		{Path: "pc/rt/CI/" + c.Icon + ".icb.szs", Kind: "CraneIcon", Name: c.Icon},
	}
	for _, f := range c.FixedObjects {
		parts = append(parts, ArchiveFile{Path: "pc/rt/FO/" + f + ".fob.szs", Kind: "FixedObject", Name: f})
	}
	for _, a := range c.Attachments {
		parts = append(parts, ArchiveFile{Path: "pc/rt/At/" + a + ".atb.szs", Kind: "Attachment", Name: a})
	}
	return parts
}

// MachineParts returns a machine's parts with their data, looked up in the
// given archives in order (e.g. the source region's weekly archive, then its
// base library); parts found in none are reported in missing.
func MachineParts(c *CraneInstance, archives ...[]SARCEntry) (parts []ArchiveFile, missing []string) {
	for _, want := range MachinePartPaths(c) {
		found := false
		for _, arch := range archives {
			for _, e := range arch {
				if e.Name == want.Path {
					want.Data = e.Data
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			parts = append(parts, want)
		} else {
			missing = append(missing, want.Path)
		}
	}
	return parts, missing
}

// ParseCraneInstanceFile and ParsePrizeFile read the Yaz0-compressed files
// stored in the archives.
func ParseCraneInstanceFile(szs []byte) (*CraneInstance, error) {
	d, err := DecompressYaz0(szs)
	if err != nil {
		return nil, err
	}
	return ParseCraneInstance(d)
}

func ParsePrizeFile(szs []byte) (*Prize, error) {
	d, err := DecompressYaz0(szs)
	if err != nil {
		return nil, err
	}
	return ParsePrize(d)
}

// CategoryPath is where a collection book (category) is stored.
func CategoryPath(name string) string { return "pc/rt/Ca/" + name + ".cab.szs" }

// FileLookup finds a file in a region's archives (weekly crane archive and
// base library).
type FileLookup func(path string) ([]byte, bool)

// MachineBundle collects everything needed to play a Nintendo machine in
// another region: the machine (.cib), its parts, its badges and their
// collection books. Files src can't provide are reported in missing; then the
// machine can't be copied.
func MachineBundle(c *CraneInstance, src FileLookup) (files []ArchiveFile, missing []string) {
	add := func(f ArchiveFile) {
		if d, ok := src(f.Path); ok {
			f.Data = d
			files = append(files, f)
		} else {
			missing = append(missing, f.Path)
		}
	}
	add(ArchiveFile{Path: CraneInstancePath(c.Name), Kind: "CraneInstance", Name: c.Name})
	for _, p := range MachinePartPaths(c) {
		add(p)
	}
	categories := map[string]bool{}
	for _, name := range c.Prizes {
		path := PrizePath(name)
		d, ok := src(path)
		if !ok {
			missing = append(missing, path)
			continue
		}
		files = append(files, ArchiveFile{Path: path, Data: d, Kind: "Prize", Name: name})
		if p, err := ParsePrizeFile(d); err == nil && p.CategoryName() != "" && !categories[p.CategoryName()] {
			categories[p.CategoryName()] = true
			add(ArchiveFile{Path: CategoryPath(p.CategoryName()), Kind: "Category", Name: p.CategoryName()})
		}
	}
	return files, missing
}
