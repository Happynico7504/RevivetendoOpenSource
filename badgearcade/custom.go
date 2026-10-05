package badgearcade

// Adding custom badges and crane machines to a weekly crane archive
// (sharc/<week>.sarc inside data_v131.dat). Every file must also be listed in
// pc/PrizeCollection.xml, the archive's registry of all prizes and machines.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PrizePath and CraneInstancePath are where the game looks for the files.
func PrizePath(name string) string         { return "pc/rt/Pr/" + name + ".prb.szs" }
func CraneInstancePath(name string) string { return "pc/ci/" + name + ".cib.szs" }

const prizeCollectionPath = "pc/PrizeCollection.xml"

var prizeCountRe = regexp.MustCompile(`<Prizes count="(\d+)">`)

// RegisterInPrizeCollection adds prize and crane-instance names to
// PrizeCollection.xml (skipping ones already listed) and updates the prize
// count. Nintendo's file is UTF-8 with a BOM and CRLF line endings.
func RegisterInPrizeCollection(xml string, prizes, instances []string) (string, error) {
	add := func(xml, closing, element string, names []string) (string, int, error) {
		i := strings.Index(xml, closing)
		if i < 0 {
			return "", 0, fmt.Errorf("badgearcade: PrizeCollection.xml has no %s", closing)
		}
		// Keep the indentation of the closing tag's line.
		lineStart := strings.LastIndex(xml[:i], "\n") + 1
		added := 0
		var b strings.Builder
		for _, n := range names {
			entry := fmt.Sprintf(`<%s name="%s" />`, element, n)
			if strings.Contains(xml, entry) {
				continue
			}
			b.WriteString("    " + entry + "\r\n")
			added++
		}
		return xml[:lineStart] + b.String() + xml[lineStart:], added, nil
	}
	xml, addedPrizes, err := add(xml, "</Prizes>", "Prize", prizes)
	if err != nil {
		return "", err
	}
	if m := prizeCountRe.FindStringSubmatch(xml); m != nil {
		n, _ := strconv.Atoi(m[1])
		xml = strings.Replace(xml, m[0], fmt.Sprintf(`<Prizes count="%d">`, n+addedPrizes), 1)
	}
	xml, _, err = add(xml, "</CraneInstances>", "CraneInstance", instances)
	return xml, err
}

// AddCustomContent adds badges and crane machines to a weekly crane archive
// and registers them, returning the rebuilt archive. Files with the same name
// are replaced.
func AddCustomContent(weekly []byte, prizes []*Prize, instances []*CraneInstance) ([]byte, error) {
	entries, err := ParseSARC(weekly)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	var prizeNames, instanceNames []string
	for _, p := range prizes {
		files[PrizePath(p.Name())] = CompressYaz0(p.Bytes())
		prizeNames = append(prizeNames, p.Name())
	}
	for _, c := range instances {
		raw, err := c.Bytes()
		if err != nil {
			return nil, err
		}
		files[CraneInstancePath(c.Name)] = CompressYaz0(raw)
		instanceNames = append(instanceNames, c.Name)
	}
	found := false
	out := make([]SARCEntry, 0, len(entries)+len(files))
	for _, e := range entries {
		if _, replaced := files[e.Name]; replaced {
			continue
		}
		if e.Name == prizeCollectionPath {
			xml, err := RegisterInPrizeCollection(string(e.Data), prizeNames, instanceNames)
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
	for name, data := range files {
		out = append(out, SARCEntry{Name: name, Data: data})
	}
	// Nintendo's weekly archives use 128-byte alignment (rebuilds byte-exact).
	return BuildSARC(out, 128, 128), nil
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
