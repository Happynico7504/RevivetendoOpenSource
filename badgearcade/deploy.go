package badgearcade

// Helpers for deployments: picking Nintendo machines as templates and filling
// them with custom badges.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// WeeklyArchiveIndex returns the index of the weekly crane archive
// (sharc/<week>.sarc) among data_v131.dat's SARC entries.
func WeeklyArchiveIndex(entries []SARCEntry) (int, error) {
	for i, e := range entries {
		if strings.HasPrefix(e.Name, "sharc/") && strings.HasSuffix(e.Name, ".sarc") {
			return i, nil
		}
	}
	return -1, errors.New("badgearcade: package has no crane archive")
}

// CraneTemplate describes a Nintendo machine that can be reused as a layout.
type CraneTemplate struct {
	Name         string
	Stage        string
	Icon         string
	Type         uint32
	PrizeSlots   int // badges on the machine
	Attachments  []string
	FixedObjects []string
	Difficult    bool // has Nintendo's "Difficult"/"Hard" obstacles
}

// CraneTemplates lists the machines in a weekly crane archive that are usable
// as deployment templates (regular crane games with at least one prize).
func CraneTemplates(weekly []byte) ([]CraneTemplate, error) {
	entries, err := ParseSARC(weekly)
	if err != nil {
		return nil, err
	}
	var out []CraneTemplate
	for _, e := range entries {
		if !strings.HasPrefix(e.Name, "pc/ci/") || !strings.HasSuffix(e.Name, ".cib.szs") {
			continue
		}
		c, err := ParseCraneInstanceFile(e.Data)
		if err != nil || c.Availability != 0 || len(c.MachinePrizes) == 0 {
			continue
		}
		t := CraneTemplate{
			Name: c.Name, Stage: c.Crane, Icon: c.Icon, Type: c.Type, PrizeSlots: len(c.MachinePrizes),
			Attachments: c.Attachments, FixedObjects: c.FixedObjects,
		}
		for _, f := range c.FixedObjects {
			if strings.Contains(f, "Difficult") || strings.Contains(f, "Hard") {
				t.Difficult = true
			}
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// TemplatePrizeCategory returns the category of a template machine's first
// prize, so custom badges can file under an existing collection book.
func TemplatePrizeCategory(weekly []byte, template string) string {
	entries, err := ParseSARC(weekly)
	if err != nil {
		return ""
	}
	var first string
	for _, e := range entries {
		if e.Name == CraneInstancePath(template) {
			if c, err := ParseCraneInstanceFile(e.Data); err == nil && len(c.Prizes) > 0 {
				first = c.Prizes[0]
			}
		}
	}
	for _, e := range entries {
		if first != "" && e.Name == PrizePath(first) {
			if p, err := ParsePrizeFile(e.Data); err == nil {
				return p.CategoryName()
			}
		}
	}
	return ""
}

// DeployMachine clones a template machine from the weekly archive as a new
// machine holding the given badges: the template's prize spots are filled
// with the badges in turn (so 1 badge fills every spot).
func DeployMachine(weekly []byte, template, name string, id uint32, badges []*Prize) (*CraneInstance, error) {
	if len(badges) == 0 {
		return nil, errors.New("badgearcade: a machine needs at least one badge")
	}
	if len(badges) > 20 {
		return nil, errors.New("badgearcade: at most 20 badges per machine")
	}
	if len(name) >= cibNameLen {
		return nil, fmt.Errorf("badgearcade: machine name %q too long", name)
	}
	entries, err := ParseSARC(weekly)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name != CraneInstancePath(template) {
			continue
		}
		c, err := ParseCraneInstanceFile(e.Data)
		if err != nil {
			return nil, err
		}
		c.ID, c.Name = id, name
		c.Prizes = nil
		for _, b := range badges {
			c.Prizes = append(c.Prizes, b.Name())
		}
		for i := range c.MachinePrizes {
			c.MachinePrizes[i].Index = uint32(i % len(badges))
		}
		for i := range c.CollectionPrizes {
			c.CollectionPrizes[i].Index = uint32(i % len(badges))
		}
		return c, nil
	}
	return nil, fmt.Errorf("badgearcade: template machine %s not in this archive", template)
}
