package main

// Badge Arcade deployments: approved player badges (relay-admin's
// badge_arcade_creations) placed into machines cloned from Nintendo layouts
// (badge_arcade_deployments), built into the daily SpotPass package.
//
// A deployment is "live" (everyone), "test" (test consoles only, via the
// experimental package) or "off", for an inclusive UTC date range, and fills
// either a hall slot or the daily training crane (BonusStage), on an approved
// custom stage (relay-admin's stage editor). Custom badges never appear on
// Nintendo's stages: deployments without a custom stage are skipped.
// Deployments show Friday to Sunday of the weekly lineup (see badgeArcadeLineupForEveryone).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Happynico7504/badgearcade"
)

const (
	badgeArcadeCustomBadgeBase = 900000000 // badge ID = base + creation ID (Nintendo's max: 80,100,000)
	badgeArcadeCustomCraneBase = 9000      // machine ID = base + deployment ID (Nintendo's max: 4,498)
	// Set (collection book) ID = base + the creator's set number
	// (badge_arcade_user_sets). Nintendo's books run 25-9905; IDs above that
	// are untested on hardware.
	badgeArcadeCustomSetBase = 10000
)

type badgeArcadeDeploymentBadge struct {
	CreationID int64
	Set        *badgeArcadeSet // the creator's set; every custom badge files under one
	Art        []byte
	Spec       struct {
		Names     [badgearcade.DisplayNameLanguages]string `json:"names"`
		Collision []badgearcade.Polygon                    `json:"collision"`
	}
}

// badgeArcadeDeploymentStage is an approved custom stage: 400x240 art plus the
// rail strip colour.
type badgeArcadeDeploymentStage struct {
	CreationID int64
	Art        []byte
	Tint       string
}

// badgeArcadeSet is a creator's set: every custom badge files under the set
// of the player who made it, named after their Mii ("Nico's Badges") with
// their Mii's face as the icon. Sets are made automatically, one per creator.
type badgeArcadeSet struct {
	No     int64 // stable per creator (badge_arcade_user_sets)
	PID    int64
	Icon   []byte // 64x64 PNG
	Names  [badgearcade.DisplayNameLanguages]string
	Badges int // the creator's approved badges
}

// badgeArcadeSetNames is "<name>'s Badges" in the 3DS language slots (0
// Japanese, 1 English, 2 French, 3 German, 4 Italian, 5 Spanish, 6 Chinese,
// 7 Korean, 8 Dutch, 9 Portuguese, 10 Russian, 11 Traditional Chinese).
func badgeArcadeSetNames(name string) (n [badgearcade.DisplayNameLanguages]string) {
	for i, f := range []string{"%sのバッジ", "%s's Badges", "Badges de %s", "Abzeichen von %s", "Spille di %s", "Insignias de %s",
		"%s的徽章", "%s의 배지", "Badges van %s", "Emblemas de %s", "Значки: %s", "%s的徽章"} {
		n[i] = fmt.Sprintf(f, name)
	}
	return n
}

var badgeArcadeUserSetsOnce sync.Once

// loadBadgeArcadeCreatorSet returns (making it on first use) the set of the
// creator pid, sharing loaded sets through cache. fallbackIcon (a badge's art)
// is used when the creator's Mii face can't be fetched.
func loadBadgeArcadeCreatorSet(ctx context.Context, pid int64, fallbackIcon []byte, cache map[int64]*badgeArcadeSet) *badgeArcadeSet {
	if s, ok := cache[pid]; ok {
		return s
	}
	badgeArcadeUserSetsOnce.Do(func() {
		if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS badge_arcade_user_sets (
			pid BIGINT PRIMARY KEY, no BIGSERIAL UNIQUE, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
			log.Printf("badge arcade sets: %v", err)
		}
	})
	set := &badgeArcadeSet{PID: pid}
	db.ExecContext(ctx, `INSERT INTO badge_arcade_user_sets (pid) VALUES ($1) ON CONFLICT (pid) DO NOTHING`, pid)
	if err := db.QueryRowContext(ctx, `SELECT no FROM badge_arcade_user_sets WHERE pid = $1`, pid).Scan(&set.No); err != nil {
		log.Printf("badge arcade sets: creator %d: %v", pid, err)
		return nil
	}
	var name string
	db.QueryRowContext(ctx, `SELECT COALESCE(NULLIF((SELECT mii_name FROM mii_names WHERE pid = $1), ''),
		NULLIF((SELECT pnid FROM pnid_cache WHERE pid = $1), ''), 'Revivetendo')`, pid).Scan(&name)
	set.Names = badgeArcadeSetNames(name)
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM badge_arcade_creations WHERE pid = $1 AND kind = 'badge' AND status = 'approved'`, pid).Scan(&set.Badges)
	// Icon: the Mii's face on a soft background (book icons are opaque).
	icon := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	draw.Draw(icon, icon.Bounds(), &image.Uniform{color.NRGBA{0xdb, 0xea, 0xfe, 0xff}}, image.Point{}, draw.Src)
	if face, err := fetchBadgeArcadeGalleryImage(fmt.Sprintf("%s/mii/%d/normal_face.png", badgeArcadeGalleryAssetBase, pid)); err == nil {
		draw.Draw(icon, icon.Bounds(), badgearcade.FitImage(face, 64, 64, color.Transparent), image.Point{}, draw.Over)
	} else if art, _, err := image.Decode(bytes.NewReader(fallbackIcon)); err == nil {
		log.Printf("badge arcade sets: creator %d has no Mii face (%v), using a badge as the icon", pid, err)
		draw.Draw(icon, icon.Bounds(), badgearcade.FitImage(art, 64, 64, color.Transparent), image.Point{}, draw.Over)
	}
	set.Icon = pngEncode(icon)
	cache[pid] = set
	return set
}

func pngEncode(img image.Image) []byte {
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

type badgeArcadeDeployment struct {
	ID       int64
	Name     string
	Template string
	Layout   *badgearcade.Layout // approved custom layout (on Template), nil = Nintendo's
	Slot     string              // "hall" or "training"
	Status   string
	Badges   []badgeArcadeDeploymentBadge
	Stage    *badgeArcadeDeploymentStage // nil = none (or not approved): not built
	Physics  badgeArcadePhysics          // overrides for every badge placement
}

// badgeArcadePhysics overrides the physics values of a machine's badge
// placements (nil = keep the template's). Friction and restitution are
// confirmed on hardware; Unk24 behaves as "physics strength" (probably Box2D's
// gravity scale): 0.01 barely moves, 2 lags the game. See badgearcade.Placement.
type badgeArcadePhysics struct {
	Friction    *float32 `json:"friction,omitempty"`    // Unk18
	Density     *float32 `json:"density,omitempty"`     // Unk1C
	Restitution *float32 `json:"restitution,omitempty"` // Unk20
	Unk24       *float32 `json:"unk24,omitempty"`       // physics strength: Unk24 and Unk28 (always equal in Nintendo's machines)
}

func (ph badgeArcadePhysics) apply(m *badgearcade.CraneInstance) {
	for i := range m.MachinePrizes {
		p := &m.MachinePrizes[i]
		if ph.Friction != nil {
			p.Unk18 = *ph.Friction
		}
		if ph.Density != nil {
			p.Unk1C = *ph.Density
		}
		if ph.Restitution != nil {
			p.Unk20 = *ph.Restitution
		}
		if ph.Unk24 != nil {
			p.Unk24, p.Unk28 = *ph.Unk24, *ph.Unk24
		}
	}
}

func (ph badgeArcadePhysics) set() bool {
	return ph.Friction != nil || ph.Density != nil || ph.Restitution != nil || ph.Unk24 != nil
}

// loadBadgeArcadeDeployments returns the deployments active on day: live ones,
// plus test ones when includeTest. Only approved badges are used.
func loadBadgeArcadeDeployments(ctx context.Context, day time.Time, includeTest bool) ([]badgeArcadeDeployment, error) {
	statuses := []string{"live"}
	if includeTest {
		statuses = append(statuses, "test")
	}
	date := day.Format("2006-01-02")
	rows, err := db.QueryContext(ctx, `SELECT d.id, d.name, d.template, d.slot, d.status, c.id, c.art, c.spec->>'tint', COALESCE(d.physics, '{}'),
		COALESCE(l.spec->'layout', 'null')
		FROM badge_arcade_deployments d
		LEFT JOIN badge_arcade_creations c ON c.id = d.stage_id AND c.status = 'approved' AND c.kind = 'stage'
		LEFT JOIN badge_arcade_creations l ON l.id = d.layout_id AND l.status = 'approved' AND l.kind = 'crane'
		WHERE d.status = ANY($1) AND d.start_date <= $2::date AND d.end_date >= $2::date ORDER BY d.id`, "{"+strings.Join(statuses, ",")+"}", date)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") { // relay-admin hasn't created the tables yet
			return nil, nil
		}
		return nil, err
	}
	var deps []badgeArcadeDeployment
	sets := map[int64]*badgeArcadeSet{}
	for rows.Next() {
		var d badgeArcadeDeployment
		var stageID sql.NullInt64
		var stageArt []byte
		var tint sql.NullString
		var physics, layout []byte
		if rows.Scan(&d.ID, &d.Name, &d.Template, &d.Slot, &d.Status, &stageID, &stageArt, &tint, &physics, &layout) == nil {
			json.Unmarshal(physics, &d.Physics)
			if string(layout) != "null" {
				var l badgearcade.Layout
				if json.Unmarshal(layout, &l) == nil && l.Template != "" {
					d.Layout, d.Template = &l, l.Template
				}
			}
			if stageID.Valid {
				d.Stage = &badgeArcadeDeploymentStage{CreationID: stageID.Int64, Art: stageArt, Tint: tint.String}
			}
			deps = append(deps, d)
		}
	}
	rows.Close()
	for i := range deps {
		brows, err := db.QueryContext(ctx, `SELECT c.id, c.pid, c.art, c.spec FROM badge_arcade_deployment_badges b
			JOIN badge_arcade_creations c ON c.id = b.creation_id
			WHERE b.deployment_id = $1 AND c.status = 'approved' AND c.kind = 'badge' ORDER BY b.position, c.id`, deps[i].ID)
		if err != nil {
			return nil, err
		}
		var pids []int64
		for brows.Next() {
			var b badgeArcadeDeploymentBadge
			var pid int64
			var spec []byte
			if brows.Scan(&b.CreationID, &pid, &b.Art, &spec) == nil {
				json.Unmarshal(spec, &b.Spec)
				deps[i].Badges = append(deps[i].Badges, b)
				pids = append(pids, pid)
			}
		}
		brows.Close()
		for j := range deps[i].Badges {
			deps[i].Badges[j].Set = loadBadgeArcadeCreatorSet(ctx, pids[j], deps[i].Badges[j].Art, sets)
		}
	}
	return deps, nil
}

// badgeArcadeDeploymentsFingerprint identifies what a set of deployments puts
// into a package, so "Publish now" only rebuilds packages that change.
func badgeArcadeDeploymentsFingerprint(deps []badgeArcadeDeployment) string {
	h := sha256.New()
	for _, d := range deps {
		fmt.Fprintf(h, "%d|%s|%s|%s|", d.ID, d.Template, d.Slot, d.Status)
		if d.Stage != nil {
			art := sha256.Sum256(d.Stage.Art)
			fmt.Fprintf(h, "stage:%d:%x:%s|", d.Stage.CreationID, art[:8], d.Stage.Tint)
		}
		if d.Layout != nil {
			l, _ := json.Marshal(d.Layout)
			fmt.Fprintf(h, "layout:%x|", sha256.Sum256(l))
		}
		for _, b := range d.Badges {
			if b.Set != nil {
				names, _ := json.Marshal(b.Set.Names)
				fmt.Fprintf(h, "set:%d:%d:%x:%x|", b.Set.No, b.Set.Badges, sha256.Sum256(b.Set.Icon), sha256.Sum256(names))
			}
		}
		if d.Physics.set() { // only when set, so existing fingerprints stay the same
			ph, _ := json.Marshal(d.Physics)
			fmt.Fprintf(h, "physics:%s|", ph)
		}
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
func badgeArcadeDeployedStageName(creationID int64) string {
	return fmt.Sprintf("CrSt_Rvt_S%07d", creationID)
}

// badgeArcadeStageTint parses a stage's "#rrggbb" strip colour (grey if unset).
func badgeArcadeStageTint(s string) color.NRGBA {
	if v, err := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32); err == nil && len(s) == 7 {
		return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}
	}
	return color.NRGBA{0x9c, 0xa3, 0xaf, 255}
}

// buildBadgeArcadeSet builds a creator's collection book (.cab).
func buildBadgeArcadeSet(set *badgeArcadeSet) (badgearcade.ArchiveFile, error) {
	icon, _, err := image.Decode(bytes.NewReader(set.Icon))
	if err != nil {
		return badgearcade.ArchiveFile{}, fmt.Errorf("bad icon: %v", err)
	}
	name := fmt.Sprintf("Rvt_Set%07d", set.No)
	pages := uint32(max(1, (set.Badges+5)/6)) // inferred; see badgearcade.CollectionBook
	book, err := badgearcade.BuildCollectionBook(badgearcade.CollectionBookSpec{
		ID: uint32(badgeArcadeCustomSetBase + set.No), Name: name, Names: set.Names, Icon: icon,
		TotalBadges: uint32(set.Badges), Pages: pages,
	})
	if err != nil {
		return badgearcade.ArchiveFile{}, err
	}
	return badgearcade.ArchiveFile{Path: badgearcade.CategoryPath(name), Data: badgearcade.CompressYaz0(book.Bytes()), Kind: "Category", Name: name}, nil
}

// badgeArcadeLookupAnyRegion finds a file in any region's archives.
func badgeArcadeLookupAnyRegion(path string) ([]byte, bool) {
	for _, prefix := range badgeArcadeGalleryPrefixes {
		if r, err := loadBadgeArcadeRegion(prefix); err == nil {
			if d, ok := r.lookup(path); ok {
				return d, true
			}
		}
	}
	return nil, false
}

// buildBadgeArcadeStage builds a custom stage's .crb archive file.
func buildBadgeArcadeStage(st *badgeArcadeDeploymentStage) (badgearcade.ArchiveFile, error) {
	img, _, err := image.Decode(bytes.NewReader(st.Art))
	if err != nil {
		return badgearcade.ArchiveFile{}, fmt.Errorf("bad stage art: %v", err)
	}
	name := badgeArcadeDeployedStageName(st.CreationID)
	crb, err := badgearcade.BuildCraneStage(name, img, badgeArcadeStageTint(st.Tint))
	if err != nil {
		return badgearcade.ArchiveFile{}, err
	}
	raw, err := crb.Bytes()
	if err != nil {
		return badgearcade.ArchiveFile{}, err
	}
	return badgearcade.ArchiveFile{Path: badgearcade.CraneStagePath(name), Data: badgearcade.CompressYaz0(raw), Kind: "Crane", Name: name}, nil
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
		if d.Stage == nil {
			log.Printf("badge arcade deploy: deployment %d has no approved custom stage, skipped", d.ID)
			continue
		}
		// The template machine comes from this region's archive, or - when this
		// region doesn't have it - from one that does.
		source := weekly
		var src *badgeArcadeRegion
		if !target.hasWeekly(badgearcade.CraneInstancePath(d.Template)) {
			if src = badgeArcadeRegionWithTemplate(d.Template); src == nil {
				log.Printf("badge arcade deploy: deployment %d: template %s is in no region's archive, skipped", d.ID, d.Template)
				continue
			}
			source = src.weeklyRaw
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
			// Custom badges never file under Nintendo's sets: no creator set, no badge.
			if b.Set == nil {
				log.Printf("badge arcade deploy: creation %d: no creator set, skipped", b.CreationID)
				continue
			}
			book, err := buildBadgeArcadeSet(b.Set)
			if err != nil {
				log.Printf("badge arcade deploy: creation %d: set %d: %v, skipped", b.CreationID, b.Set.No, err)
				continue
			}
			cat := book.Name
			if !copied[book.Path] {
				copied[book.Path] = true
				extra = append(extra, book)
			}
			p := badgearcade.BuildPrize(badgearcade.PrizeSpec{
				BadgeID:   uint32(badgeArcadeCustomBadgeBase + b.CreationID),
				FileName:  badgeArcadeDeployedBadgeName(b.CreationID),
				Category:  cat,
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
		var m *badgearcade.CraneInstance
		if d.Layout != nil {
			m, err = badgearcade.DeployLayout(source, *d.Layout, name, uint32(badgeArcadeCustomCraneBase+d.ID), badges)
		} else {
			m, err = badgearcade.DeployMachine(source, d.Template, name, uint32(badgeArcadeCustomCraneBase+d.ID), badges)
		}
		if err != nil {
			log.Printf("badge arcade deploy: %s: deployment %d: %v", prefix, d.ID, err)
			continue
		}
		stage, err := buildBadgeArcadeStage(d.Stage)
		if err != nil {
			log.Printf("badge arcade deploy: deployment %d: stage %d: %v, skipped", d.ID, d.Stage.CreationID, err)
			continue
		}
		m.Crane = stage.Name
		d.Physics.apply(m)
		// Parts this region lacks (icon, obstacles, attachments) come from the
		// template's region, else from any region: a layout may use obstacles
		// that only shipped elsewhere.
		var need []badgearcade.ArchiveFile
		var missing []string
		for _, part := range badgearcade.MachinePartPaths(m) {
			if part.Path == stage.Path || target.has(part.Path) || copied[part.Path] {
				continue
			}
			data, ok := []byte(nil), false
			if src != nil {
				data, ok = src.lookup(part.Path)
			}
			if !ok {
				data, ok = badgeArcadeLookupAnyRegion(part.Path)
			}
			if !ok {
				missing = append(missing, part.Path)
				continue
			}
			part.Data = data
			need = append(need, part)
		}
		if len(missing) > 0 {
			log.Printf("badge arcade deploy: %s: deployment %d: parts in no region: %v, skipped", prefix, d.ID, missing)
			continue
		}
		for _, part := range append(need, stage) {
			if !copied[part.Path] {
				copied[part.Path] = true
				extra = append(extra, part)
			}
		}
		if len(need) > 0 {
			log.Printf("badge arcade deploy: %s: deployment %d: %d parts copied from other regions", prefix, d.ID, len(need))
		}
		machines = append(machines, m)
		if d.Slot == "training" {
			training = name // the last one wins
		} else {
			hall = append(hall, name)
		}
	}
	if lineup {
		offHall, err := badgeArcadeOfficialForDay(day)
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
		// The training crane must be one of the day's hall machines: in all of
		// Nintendo's archived schedules (21 days, 3 regions) it is, and with any
		// other machine the game picks a random hall machine instead. A training
		// deployment goes to the front of the hall so the 30-machine cap keeps it.
		if training != "" {
			hall = append([]string{training}, hall...)
		}
		if len(hall) > badgeArcadeHallSlots {
			hall = hall[:badgeArcadeHallSlots]
		}
		if training == "" && len(hall) > 0 {
			training = hall[int(day.UTC().Unix()/86400)%len(hall)]
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
// deployment is active). The training crane is always one of the day's hall
// machines (the game ignores any other): a training deployment when one is
// active on a Friday to Sunday, else a hall machine picked per day. Machines a
// region lacks are copied in with their parts, badges and collection books.
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

// badgeArcadeOfficialForDay picks the day's official hall machines: the week's
// shuffle of the pool gives each weekday its own 30 machines (no repeats
// within a week).
func badgeArcadeOfficialForDay(day time.Time) (hall []badgeArcadeOfficialMachine, err error) {
	pool, err := badgeArcadeOfficialPool()
	if err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		return nil, fmt.Errorf("no official machines")
	}
	wd := badgeArcadeWeekday(day)
	monday := day.UTC().Truncate(24*time.Hour).Unix()/86400 - int64(wd)
	perm := rand.New(rand.NewPCG(uint64(monday), 0xBADC0DE)).Perm(len(pool))
	pick := func(i int) badgeArcadeOfficialMachine { return pool[perm[i%len(perm)]] }
	for k := 0; k < badgeArcadeHallSlots; k++ {
		hall = append(hall, pick(wd*badgeArcadeHallSlots+k))
	}
	return hall, nil
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
