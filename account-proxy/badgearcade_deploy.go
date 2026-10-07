package main

// Badge Arcade deployments: approved player badges (relay-admin's
// badge_arcade_creations) placed into machines cloned from Nintendo layouts
// (badge_arcade_deployments), built into the daily SpotPass package.
//
// A deployment is "live" (everyone), "test" (test consoles only, via the
// experimental package) or "off", for an inclusive UTC date range, and fills
// either a hall slot or the daily training crane (BonusStage). Deployments
// show Friday to Sunday of the weekly lineup (see badgeArcadeLineupForEveryone).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Happynico7504/badgearcade"
)

const (
	badgeArcadeCustomBadgeBase = 900000000 // badge ID = base + creation ID (Nintendo's max: 80,100,000)
	badgeArcadeCustomCraneBase = 9000      // machine ID = base + deployment ID (Nintendo's max: 4,498)
)

type badgeArcadeDeploymentBadge struct {
	CreationID int64
	Art        []byte
	Spec       struct {
		Names     [badgearcade.DisplayNameLanguages]string `json:"names"`
		Collision []badgearcade.Polygon                    `json:"collision"`
	}
}

type badgeArcadeDeployment struct {
	ID       int64
	Name     string
	Template string
	Slot     string // "hall" or "training"
	Status   string
	Badges   []badgeArcadeDeploymentBadge
}

// loadBadgeArcadeDeployments returns the deployments active on day: live ones,
// plus test ones when includeTest. Only approved badges are used.
func loadBadgeArcadeDeployments(ctx context.Context, day time.Time, includeTest bool) ([]badgeArcadeDeployment, error) {
	statuses := []string{"live"}
	if includeTest {
		statuses = append(statuses, "test")
	}
	date := day.Format("2006-01-02")
	rows, err := db.QueryContext(ctx, `SELECT id, name, template, slot, status FROM badge_arcade_deployments
		WHERE status = ANY($1) AND start_date <= $2::date AND end_date >= $2::date ORDER BY id`, "{"+strings.Join(statuses, ",")+"}", date)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") { // relay-admin hasn't created the tables yet
			return nil, nil
		}
		return nil, err
	}
	var deps []badgeArcadeDeployment
	for rows.Next() {
		var d badgeArcadeDeployment
		if rows.Scan(&d.ID, &d.Name, &d.Template, &d.Slot, &d.Status) == nil {
			deps = append(deps, d)
		}
	}
	rows.Close()
	for i := range deps {
		brows, err := db.QueryContext(ctx, `SELECT c.id, c.art, c.spec FROM badge_arcade_deployment_badges b
			JOIN badge_arcade_creations c ON c.id = b.creation_id
			WHERE b.deployment_id = $1 AND c.status = 'approved' AND c.kind = 'badge' ORDER BY b.position, c.id`, deps[i].ID)
		if err != nil {
			return nil, err
		}
		for brows.Next() {
			var b badgeArcadeDeploymentBadge
			var spec []byte
			if brows.Scan(&b.CreationID, &b.Art, &spec) == nil {
				json.Unmarshal(spec, &b.Spec)
				deps[i].Badges = append(deps[i].Badges, b)
			}
		}
		brows.Close()
	}
	return deps, nil
}

// badgeArcadeDeploymentsFingerprint identifies what a set of deployments puts
// into a package, so "Publish now" only rebuilds packages that change.
func badgeArcadeDeploymentsFingerprint(deps []badgeArcadeDeployment) string {
	h := sha256.New()
	for _, d := range deps {
		fmt.Fprintf(h, "%d|%s|%s|%s|", d.ID, d.Template, d.Slot, d.Status)
		for _, b := range d.Badges {
			spec, _ := json.Marshal(b.Spec)
			art := sha256.Sum256(b.Art)
			fmt.Fprintf(h, "%d:%x:%s;", b.CreationID, art[:8], spec)
		}
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func badgeArcadeDeployedBadgeName(creationID int64) string {
	return fmt.Sprintf("Pr_Rvt_%07d", creationID)
}
func badgeArcadeDeployedMachineName(deploymentID int64) string {
	return fmt.Sprintf("Rvt_D%05d", deploymentID)
}

// applyBadgeArcadeDeployments adds the deployments' badges and machines to the
// package's weekly crane archive and puts today's lineup on the schedule: with
// lineup, the weekly lineup (see badgeArcadeLineupForEveryone); otherwise
// Nintendo's archived week, rotating, with deployments in the first hall
// slots. Deployments that can't be built for this file set (e.g. the template
// is in no region's archive) are skipped and logged.
func applyBadgeArcadeDeployments(prefix string, kept []badgearcade.SARCEntry, schedule string, day time.Time, deps []badgeArcadeDeployment, lineup bool) (string, error) {
	if lineup && !badgeArcadeCustomDay(day) {
		deps = nil // Monday to Thursday are official
	}
	wi, err := badgearcade.WeeklyArchiveIndex(kept)
	if err != nil {
		return "", err
	}
	weekly := kept[wi].Data
	target, err := loadBadgeArcadeRegion(prefix)
	if err != nil {
		return "", err
	}
	built := map[int64]*badgearcade.Prize{}
	var prizes []*badgearcade.Prize
	var machines []*badgearcade.CraneInstance
	var extra []badgearcade.ArchiveFile
	copied := map[string]bool{}
	var hall []string
	training := ""
	for _, d := range deps {
		// The template machine comes from this region's archive, or - when this
		// region doesn't have it - from one that does, together with the parts
		// (stage, icon, objects) this region lacks.
		source := weekly
		var src *badgeArcadeRegion
		if !target.hasWeekly(badgearcade.CraneInstancePath(d.Template)) {
			if src = badgeArcadeRegionWithTemplate(d.Template); src == nil {
				log.Printf("badge arcade deploy: deployment %d: template %s is in no region's archive, skipped", d.ID, d.Template)
				continue
			}
			source = src.weeklyRaw
		}
		category := badgearcade.TemplatePrizeCategory(source, d.Template)
		if category == "" {
			log.Printf("badge arcade deploy: %s: deployment %d: template %s has no prizes, skipped", prefix, d.ID, d.Template)
			continue
		}
		var badges []*badgearcade.Prize
		for _, b := range d.Badges {
			if p, ok := built[b.CreationID]; ok {
				badges = append(badges, p)
				continue
			}
			img, _, err := image.Decode(bytes.NewReader(b.Art))
			if err != nil {
				log.Printf("badge arcade deploy: creation %d: bad art: %v", b.CreationID, err)
				continue
			}
			p := badgearcade.BuildPrize(badgearcade.PrizeSpec{
				BadgeID:   uint32(badgeArcadeCustomBadgeBase + b.CreationID),
				FileName:  badgeArcadeDeployedBadgeName(b.CreationID),
				Category:  category,
				Names:     b.Spec.Names,
				Image:     img,
				Collision: b.Spec.Collision,
			})
			built[b.CreationID] = p
			prizes = append(prizes, p)
			badges = append(badges, p)
		}
		if len(badges) == 0 {
			log.Printf("badge arcade deploy: deployment %d has no approved badges, skipped", d.ID)
			continue
		}
		name := badgeArcadeDeployedMachineName(d.ID)
		m, err := badgearcade.DeployMachine(source, d.Template, name, uint32(badgeArcadeCustomCraneBase+d.ID), badges)
		if err != nil {
			log.Printf("badge arcade deploy: %s: deployment %d: %v", prefix, d.ID, err)
			continue
		}
		if src != nil {
			parts, missing := badgearcade.MachineParts(m, src.weekly, src.base)
			if len(missing) > 0 {
				log.Printf("badge arcade deploy: %s: deployment %d: parts of %s missing in %s: %v, skipped", prefix, d.ID, d.Template, src.prefix, missing)
				continue
			}
			n := 0
			for _, part := range parts {
				if !target.has(part.Path) && !copied[part.Path] {
					copied[part.Path] = true
					extra = append(extra, part)
					n++
				}
			}
			log.Printf("badge arcade deploy: %s: deployment %d uses %s from %s (%d parts copied)", prefix, d.ID, d.Template, src.prefix, n)
		}
		machines = append(machines, m)
		if d.Slot == "training" {
			training = name // the last one wins
		} else {
			hall = append(hall, name)
		}
	}
	if lineup {
		offHall, offTraining, err := badgeArcadeOfficialForDay(day)
		if err != nil {
			return "", err
		}
		if len(hall) > 0 {
			r := rand.New(rand.NewPCG(uint64(day.Unix()/86400), 0xBADC0DE))
			r.Shuffle(len(hall), func(i, j int) { hall[i], hall[j] = hall[j], hall[i] })
		}
		var official []badgeArcadeOfficialMachine // needed in this package
		if len(hall) == 0 {
			official = offHall
			for _, m := range offHall {
				hall = append(hall, m.Name)
			}
		}
		if training == "" {
			official = append(official, offTraining)
			training = offTraining.Name
		}
		if len(hall) > badgeArcadeHallSlots {
			hall = hall[:badgeArcadeHallSlots]
		}
		copiedMachines := 0
		for _, m := range official {
			if target.machines[m.Name] != nil {
				continue
			}
			copiedMachines++
			for _, f := range m.Files {
				if !target.has(f.Path) && !copied[f.Path] {
					copied[f.Path] = true
					extra = append(extra, f)
				}
			}
		}
		log.Printf("badge arcade lineup: %s %s: %d hall machines, training %s, %d machines copied from other regions",
			prefix, day.Format("2006-01-02"), len(hall), training, copiedMachines)
	} else {
		if len(machines) == 0 {
			return schedule, nil
		}
		schedule = redateCraneSchedule(schedule, day)
	}
	if len(prizes) > 0 || len(machines) > 0 || len(extra) > 0 {
		if kept[wi].Data, err = badgearcade.AddCustomContent(weekly, prizes, machines, extra...); err != nil {
			return "", err
		}
	}
	if lineup {
		return scheduleBadgeArcadeLineup(schedule, day, hall, training), nil
	}
	today := "<DateStartText>" + day.Format("20060102") + "<"
	valueRe := regexp.MustCompile(`<Value>[^<]*</Value>`)
	next := 0
	return scheduleItemRe.ReplaceAllStringFunc(schedule, func(it string) string {
		if !strings.Contains(it, today) {
			return it
		}
		switch {
		case training != "" && strings.Contains(it, "<RegexSetName>BonusStage</RegexSetName>"):
			return valueRe.ReplaceAllString(it, "<Value>"+training+"</Value>")
		case next < len(hall) && strings.Contains(it, "<RegexSetName>DefaultStage</RegexSetName>"):
			next++
			return valueRe.ReplaceAllString(it, "<Value>"+hall[next-1]+"</Value>")
		}
		return it
	}), nil
}

// handleBadgeArcadeRebuild (internal listener, POST) rebuilds today's packages
// with a new revision, so a deployment change reaches consoles without waiting
// for midnight. relay-admin's "Publish now" calls it.
func handleBadgeArcadeRebuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	go rebuildBadgeArcadePackages(context.Background(), time.Now(), true)
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte("rebuilding\n"))
}

// badgeArcadeRegion holds one file set's archived Nintendo content, for
// copying machines between regions: the weekly crane archive and the base
// library (allbadge_v131.dat, which consoles also keep).
type badgeArcadeRegion struct {
	prefix    string
	weeklyRaw []byte
	weekly    []badgearcade.SARCEntry
	base      []badgearcade.SARCEntry
	files     map[string][]byte // weekly overrides base
	inWeekly  map[string]bool
	machines  map[string]*badgearcade.CraneInstance // playable machines (weekly and base)
}

func (r *badgeArcadeRegion) has(path string) bool       { _, ok := r.files[path]; return ok }
func (r *badgeArcadeRegion) hasWeekly(path string) bool { return r.inWeekly[path] }
func (r *badgeArcadeRegion) lookup(path string) ([]byte, bool) {
	d, ok := r.files[path]
	return d, ok
}

var (
	badgeArcadeRegionsMu sync.Mutex
	badgeArcadeRegions   = map[string]*badgeArcadeRegion{}
)

// loadBadgeArcadeRegion decrypts (once per process) a file set's Nintendo
// packages; they never change on disk.
func loadBadgeArcadeRegion(prefix string) (*badgeArcadeRegion, error) {
	badgeArcadeRegionsMu.Lock()
	defer badgeArcadeRegionsMu.Unlock()
	if r, ok := badgeArcadeRegions[prefix]; ok {
		return r, nil
	}
	key, err := loadBoss3DSKey()
	if err != nil {
		return nil, err
	}
	open := func(file string) ([]badgearcade.SARCEntry, error) {
		raw, err := os.ReadFile(badgeArcadeBossDataDir + "/" + prefix + "_" + file)
		if err != nil {
			return nil, err
		}
		_, payloads, err := badgearcade.ParseBOSS(key, raw)
		if err != nil || len(payloads) == 0 {
			return nil, fmt.Errorf("%s_%s: %v", prefix, file, err)
		}
		return badgearcade.ParseSARC(payloads[0].Content)
	}
	data, err := open("data_data_v131.dat.boss")
	if err != nil {
		return nil, err
	}
	wi, err := badgearcade.WeeklyArchiveIndex(data)
	if err != nil {
		return nil, err
	}
	r := &badgeArcadeRegion{prefix: prefix, weeklyRaw: data[wi].Data, files: map[string][]byte{}, inWeekly: map[string]bool{},
		machines: map[string]*badgearcade.CraneInstance{}}
	if r.weekly, err = badgearcade.ParseSARC(r.weeklyRaw); err != nil {
		return nil, err
	}
	if r.base, err = open("data_allbadge_v131.dat.boss"); err != nil {
		return nil, err
	}
	for _, e := range r.base {
		r.files[e.Name] = e.Data
	}
	for _, e := range r.weekly {
		r.files[e.Name], r.inWeekly[e.Name] = e.Data, true
	}
	for path, d := range r.files {
		if !strings.HasPrefix(path, "pc/ci/") || !strings.HasSuffix(path, ".cib.szs") {
			continue
		}
		if c, err := badgearcade.ParseCraneInstanceFile(d); err == nil && c.Availability == 0 && len(c.MachinePrizes) > 0 {
			r.machines[c.Name] = c
		}
	}
	badgeArcadeRegions[prefix] = r
	return r, nil
}

// badgeArcadeRegionWithTemplate finds a file set whose weekly archive has the
// template machine (in badgeArcadeGalleryPrefixes order).
func badgeArcadeRegionWithTemplate(template string) *badgeArcadeRegion {
	for _, prefix := range badgeArcadeGalleryPrefixes {
		r, err := loadBadgeArcadeRegion(prefix)
		if err == nil && r.hasWeekly(badgearcade.CraneInstancePath(template)) {
			return r
		}
	}
	return nil
}

// Weekly lineup, the same in every region: Monday to Thursday show official
// machines (from all regions' archives, shuffled per week), Friday to Sunday
// only our deployments (shuffled per day; official ones only when no hall
// deployment is active). The training crane is official unless a training
// deployment is active on a Friday to Sunday. Machines a region lacks are
// copied in with their parts, badges and collection books.
//
// badgeArcadeLineupForEveryone puts it into the stable package; when false,
// only test consoles (experimental package) get it, and the stable package
// keeps Nintendo's archived week with deployments in front.
const badgeArcadeLineupForEveryone = true

const badgeArcadeHallSlots = 30 // the game keeps the first 30 of a day's DefaultStage items

type badgeArcadeOfficialMachine struct {
	Name   string
	Source string                    // file set its bundle comes from
	Files  []badgearcade.ArchiveFile // machine, parts, badges, collection books
}

var (
	badgeArcadeOfficialOnce sync.Once
	badgeArcadeOfficial     []badgeArcadeOfficialMachine
	badgeArcadeOfficialErr  error
)

// badgeArcadeOfficialPool lists (once per process) every official machine that
// every file set has or can get a complete copy of, sorted by name. Machines
// whose files shipped in older weekly packages we don't have are left out.
func badgeArcadeOfficialPool() ([]badgeArcadeOfficialMachine, error) {
	badgeArcadeOfficialOnce.Do(func() {
		var regions []*badgeArcadeRegion
		for _, prefix := range badgeArcadeGalleryPrefixes {
			r, err := loadBadgeArcadeRegion(prefix)
			if err != nil {
				badgeArcadeOfficialErr = err
				return
			}
			regions = append(regions, r)
		}
		names := map[string]bool{}
		for _, r := range regions {
			for n := range r.machines {
				names[n] = true
			}
		}
		// Parts and badges are looked up in the machine's own region first, then
		// in the others (a machine's files may have shipped to another region only).
		for n := range names {
			m := badgeArcadeOfficialMachine{Name: n}
			for _, r := range regions {
				c := r.machines[n]
				if c == nil {
					continue
				}
				lookup := func(path string) ([]byte, bool) {
					if d, ok := r.files[path]; ok {
						return d, true
					}
					for _, o := range regions {
						if d, ok := o.files[path]; ok {
							return d, true
						}
					}
					return nil, false
				}
				if files, missing := badgearcade.MachineBundle(c, lookup); len(missing) == 0 {
					m.Source, m.Files = r.prefix, files
					break
				}
			}
			everywhere := true
			for _, r := range regions {
				everywhere = everywhere && (r.machines[n] != nil || m.Files != nil)
			}
			if everywhere {
				badgeArcadeOfficial = append(badgeArcadeOfficial, m)
			}
		}
		sort.Slice(badgeArcadeOfficial, func(i, j int) bool { return badgeArcadeOfficial[i].Name < badgeArcadeOfficial[j].Name })
	})
	return badgeArcadeOfficial, badgeArcadeOfficialErr
}

// badgeArcadeWeekday is 0 for Monday to 6 for Sunday (UTC).
func badgeArcadeWeekday(day time.Time) int { return (int(day.UTC().Weekday()) + 6) % 7 }

// badgeArcadeCustomDay: Friday to Sunday belong to our deployments.
func badgeArcadeCustomDay(day time.Time) bool { return badgeArcadeWeekday(day) >= 4 }

// badgeArcadeOfficialForDay picks the day's official hall machines and training
// machine: the week's shuffle of the pool gives each weekday its own 30
// machines (no repeats within a week) and training machine.
func badgeArcadeOfficialForDay(day time.Time) (hall []badgeArcadeOfficialMachine, training badgeArcadeOfficialMachine, err error) {
	pool, err := badgeArcadeOfficialPool()
	if err != nil {
		return nil, training, err
	}
	if len(pool) == 0 {
		return nil, training, fmt.Errorf("no official machines")
	}
	wd := badgeArcadeWeekday(day)
	monday := day.UTC().Truncate(24*time.Hour).Unix()/86400 - int64(wd)
	perm := rand.New(rand.NewPCG(uint64(monday), 0xBADC0DE)).Perm(len(pool))
	pick := func(i int) badgeArcadeOfficialMachine { return pool[perm[i%len(perm)]] }
	for k := 0; k < badgeArcadeHallSlots; k++ {
		hall = append(hall, pick(wd*badgeArcadeHallSlots+k))
	}
	return hall, pick(7*badgeArcadeHallSlots + wd), nil
}

// scheduleBadgeArcadeLineup dates the given machines for today: hall machines
// into the first DefaultStage items, training into the first BonusStage item;
// every other stage item keeps Nintendo's (expired) dates. The crane archive
// (PrizeCollection) gets an open window.
func scheduleBadgeArcadeLineup(schedule string, day time.Time, hall []string, training string) string {
	today, tomorrow := day.Format("20060102"), day.Add(24*time.Hour).Format("20060102")
	window := func(item, from, to string) string {
		item = regexp.MustCompile(`<DateStartText>\d{8}<`).ReplaceAllString(item, "<DateStartText>"+from+"<")
		return regexp.MustCompile(`<DateExpireText>\d{8}<`).ReplaceAllString(item, "<DateExpireText>"+to+"<")
	}
	valueRe := regexp.MustCompile(`<Value>[^<]*</Value>`)
	next, bonusDone := 0, false
	return scheduleItemRe.ReplaceAllStringFunc(schedule, func(it string) string {
		switch {
		case strings.Contains(it, "<RegexSetName>PrizeCollection</RegexSetName>"):
			return window(it, "20260101", "20991231")
		case strings.Contains(it, "<RegexSetName>DefaultStage</RegexSetName>") && next < len(hall):
			next++
			return valueRe.ReplaceAllString(window(it, today, tomorrow), "<Value>"+hall[next-1]+"</Value>")
		case strings.Contains(it, "<RegexSetName>BonusStage</RegexSetName>") && training != "" && !bonusDone:
			bonusDone = true
			return valueRe.ReplaceAllString(window(it, today, tomorrow), "<Value>"+training+"</Value>")
		}
		return it
	})
}
