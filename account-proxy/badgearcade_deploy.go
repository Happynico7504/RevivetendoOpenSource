package main

// Badge Arcade deployments: approved player badges (relay-admin's
// badge_arcade_creations) placed into machines cloned from Nintendo layouts
// (badge_arcade_deployments), built into the daily SpotPass package.
//
// A deployment is "live" (everyone), "test" (test consoles only, via the
// experimental package) or "off", for an inclusive UTC date range, and fills
// either a hall slot or the daily training crane (BonusStage). Putting machines
// on the schedule needs today's lineup dated, so while any deployment applies
// the package carries Nintendo's archived week as a rotating lineup
// (redateCraneSchedule), with deployed machines in its first hall slots.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"log"
	"net/http"
	"regexp"
	"strings"
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
// package's weekly crane archive and schedules them on today's (already dated)
// lineup. Deployments that can't be built for this file set (e.g. the template
// isn't in this region's archive) are skipped and logged.
func applyBadgeArcadeDeployments(prefix string, kept []badgearcade.SARCEntry, schedule string, day time.Time, deps []badgeArcadeDeployment) (string, error) {
	wi, err := badgearcade.WeeklyArchiveIndex(kept)
	if err != nil {
		return "", err
	}
	weekly := kept[wi].Data
	built := map[int64]*badgearcade.Prize{}
	var prizes []*badgearcade.Prize
	var machines []*badgearcade.CraneInstance
	var hall []string
	training := ""
	for _, d := range deps {
		category := badgearcade.TemplatePrizeCategory(weekly, d.Template)
		if category == "" {
			log.Printf("badge arcade deploy: %s: deployment %d: template %s not in this archive, skipped", prefix, d.ID, d.Template)
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
		m, err := badgearcade.DeployMachine(weekly, d.Template, name, uint32(badgeArcadeCustomCraneBase+d.ID), badges)
		if err != nil {
			log.Printf("badge arcade deploy: %s: deployment %d: %v", prefix, d.ID, err)
			continue
		}
		machines = append(machines, m)
		if d.Slot == "training" {
			training = name // the last one wins
		} else {
			hall = append(hall, name)
		}
	}
	if len(machines) == 0 {
		return schedule, nil
	}
	if kept[wi].Data, err = badgearcade.AddCustomContent(weekly, prizes, machines); err != nil {
		return "", err
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
