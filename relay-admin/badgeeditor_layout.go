package main

// Badge Arcade layout editor backend: custom machine layouts (creation kind
// "crane") built on Nintendo template machines - see badgearcade/layout.go.
// Nintendo's parts (machines, obstacles, attachments, stages, icons) come from
// all three regions' archived packages, so the editor offers everything
// account-proxy can put into any region's package.
//
//	/my/badge-arcade/layout.html                   the editor
//	/my/badge-arcade/api/layout/templates          template machines
//	/my/badge-arcade/api/layout/template?name=X    a template as a layout, plus its joints
//	/my/badge-arcade/api/layout/obstacles          the obstacle and attachment palettes (size, collision), limits
//	/my/badge-arcade/api/layout/part/{kind}/{name}.png  fob, atb, stage (top screen), icon
//	/my/badge-arcade/api/preview-layout            server render (the real placement maths)

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Happynico7504/badgearcade"
)

// badgeArcadeParts holds the compressed part files of every region (paths as
// in the archives; the first region that has a path wins - they're identical).
type badgeArcadeParts struct {
	files       map[string][]byte
	obstacles   []badgeArcadeObstacleInfo
	attachments []badgeArcadeObstacleInfo
	pngs        sync.Map // "kind/name" -> []byte
}

type badgeArcadeObstacleInfo struct {
	Name      string                `json:"name"`
	W         int                   `json:"w"`
	H         int                   `json:"h"`
	Invisible bool                  `json:"invisible"`
	Polygons  []badgearcade.Polygon `json:"polys"`
}

var (
	badgeArcadePartsOnce sync.Once
	badgeArcadePartsData *badgeArcadeParts
	badgeArcadePartsErr  error
)

var badgeArcadePartPrefixes = []string{"pc/ci/", "pc/rt/FO/", "pc/rt/At/", "pc/rt/Cr/", "pc/rt/CI/"}

func loadBadgeArcadeParts() (*badgeArcadeParts, error) {
	badgeArcadePartsOnce.Do(func() {
		keyPath := os.Getenv("BOSS3DS_KEY_FILE")
		if keyPath == "" {
			keyPath = "/home/nico/boss3ds_key.hex"
		}
		key, err := badgearcade.LoadBOSSKey(keyPath)
		if err != nil {
			badgeArcadePartsErr = err
			return
		}
		p := &badgeArcadeParts{files: map[string][]byte{}}
		keep := func(entries []badgearcade.SARCEntry) {
			for _, e := range entries {
				if _, seen := p.files[e.Name]; seen {
					continue
				}
				for _, pre := range badgeArcadePartPrefixes {
					if strings.HasPrefix(e.Name, pre) {
						p.files[e.Name] = e.Data
					}
				}
			}
		}
		for _, region := range badgeArcadeDeployRegions {
			for _, file := range []string{"data_data_v131.dat.boss", "data_allbadge_v131.dat.boss"} {
				raw, err := os.ReadFile("/home/nico/badgearcade-boss-data/" + region.Prefix + "_" + file)
				if err != nil {
					badgeArcadePartsErr = err
					return
				}
				_, payloads, err := badgearcade.ParseBOSS(key, raw)
				if err != nil {
					badgeArcadePartsErr = err
					return
				}
				entries, err := badgearcade.ParseSARC(payloads[0].Content)
				if err != nil {
					badgeArcadePartsErr = err
					return
				}
				// The weekly crane archive first: it overrides the base library.
				if wi, err := badgearcade.WeeklyArchiveIndex(entries); err == nil {
					if weekly, err := badgearcade.ParseSARC(entries[wi].Data); err == nil {
						keep(weekly)
					}
				}
				keep(entries)
			}
		}
		for path, d := range p.files {
			switch {
			case strings.HasPrefix(path, "pc/rt/FO/"):
				f, err := badgearcade.ParseFixedObjectFile(d)
				if err != nil {
					continue
				}
				polys, _ := f.Polygons()
				p.obstacles = append(p.obstacles, badgeArcadeObstacleInfo{Name: f.Name, W: f.Width, H: f.Height,
					Invisible: strings.Contains(f.Name, "Invisible"), Polygons: polys})
			case strings.HasPrefix(path, "pc/rt/At/"):
				a, err := badgearcade.ParseAttachmentFile(d)
				if err != nil {
					continue
				}
				polys, _ := a.Polygons()
				p.attachments = append(p.attachments, badgeArcadeObstacleInfo{Name: a.Name, W: a.Width, H: a.Height,
					Invisible: strings.Contains(a.Name, "Invisible"), Polygons: polys})
			}
		}
		for _, list := range [][]badgeArcadeObstacleInfo{p.obstacles, p.attachments} {
			sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		}
		log.Printf("badge editor: layout parts loaded (%d files, %d obstacles, %d attachments)", len(p.files), len(p.obstacles), len(p.attachments))
		badgeArcadePartsData = p
	})
	return badgeArcadePartsData, badgeArcadePartsErr
}

func (p *badgeArcadeParts) lookup(path string) ([]byte, bool) {
	d, ok := p.files[path]
	return d, ok
}

func (p *badgeArcadeParts) hasPart(kind, name string) bool {
	path := badgearcade.FixedObjectPath(name)
	if kind == badgearcade.LayoutAttachment {
		path = badgearcade.AttachmentPath(name)
	}
	_, ok := p.files[path]
	return ok
}

// template returns a fresh copy of a template machine.
func (p *badgeArcadeParts) template(name string) (*badgearcade.CraneInstance, error) {
	d, ok := p.files[badgearcade.CraneInstancePath(name)]
	if !ok {
		return nil, fmt.Errorf("unknown template machine %s", name)
	}
	return badgearcade.ParseCraneInstanceFile(d)
}

// partPNG renders (and caches) a part as PNG: fob/atb texture, stage top
// screen, or crane icon.
func (p *badgeArcadeParts) partPNG(kind, name string) ([]byte, error) {
	key := kind + "/" + name
	if v, ok := p.pngs.Load(key); ok {
		return v.([]byte), nil
	}
	var img image.Image
	var err error
	switch kind {
	case "fob":
		var f *badgearcade.FixedObject
		if d, ok := p.files[badgearcade.FixedObjectPath(name)]; !ok {
			err = errors.New("unknown obstacle")
		} else if f, err = badgearcade.ParseFixedObjectFile(d); err == nil {
			img = f.Image()
		}
	case "atb":
		var a *badgearcade.Attachment
		if d, ok := p.files[badgearcade.AttachmentPath(name)]; !ok {
			err = errors.New("unknown attachment")
		} else if a, err = badgearcade.ParseAttachmentFile(d); err == nil {
			img = a.Image()
		}
	case "stage":
		var s *badgearcade.CraneStage
		if d, ok := p.files[badgearcade.CraneStagePath(name)]; !ok {
			err = errors.New("unknown stage")
		} else if s, err = badgearcade.ParseCraneStageFile(d); err == nil {
			img = s.Screen()
		}
	case "icon":
		var c *badgearcade.CraneIcon
		if d, ok := p.files[badgearcade.CraneIconPath(name)]; !ok {
			err = errors.New("unknown icon")
		} else if c, err = badgearcade.ParseCraneIconFile(d); err == nil {
			img = c.Image()
		}
	default:
		err = errors.New("unknown part kind")
	}
	if err != nil {
		return nil, err
	}
	b := pngBytes(img)
	p.pngs.Store(key, b)
	return b, nil
}

// badgeEditorLayoutSpec is what the editor stores for a layout.
type badgeEditorLayoutSpec struct {
	Layout badgearcade.Layout `json:"layout"`
	// Preview only: one of the player's stages (0 = the template's stage) and
	// one of their badges to draw the spots with (0 = a placeholder).
	Stage int64 `json:"stage,omitempty"`
	Badge int64 `json:"badge,omitempty"`
}

// validateLayout checks a layout against the template list and the parts
// account-proxy can deliver.
func validateLayout(l *badgearcade.Layout) error {
	templates, err := badgeArcadeDeployTemplates()
	if err != nil {
		return errors.New("template machines unavailable")
	}
	known := false
	for _, t := range templates {
		known = known || t.Name == l.Template
	}
	if !known {
		return fmt.Errorf("unknown template machine %q", l.Template)
	}
	if err := l.Validate(); err != nil {
		return err
	}
	parts, err := loadBadgeArcadeParts()
	if err != nil {
		return errors.New("machine parts unavailable")
	}
	for _, o := range l.Objects {
		if o.Kind != badgearcade.LayoutBadge && !parts.hasPart(o.Kind, o.Part) {
			return fmt.Errorf("unknown %s %q", o.Kind, o.Part)
		}
	}
	return nil
}

// badgeSpotPlaceholder is drawn for badge spots without a chosen badge: a
// translucent disc with a ring, the size of a badge filling its 128x128 art.
var badgeSpotPlaceholder = func() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			d := math.Hypot(float64(x)-63.5, float64(y)-63.5)
			switch {
			case d < 54:
				img.SetNRGBA(x, y, color.NRGBA{255, 255, 255, 90})
			case d < 60:
				img.SetNRGBA(x, y, color.NRGBA{99, 102, 241, 230})
			}
		}
	}
	return img
}()

// renderLayout draws a layout as the 400x240 top screen: stage (the given
// one, else the template's), obstacles, the template's attachments, and badge
// spots drawn with spot (128x128 base).
func renderLayout(l badgearcade.Layout, stage, spot image.Image) (*image.NRGBA, error) {
	parts, err := loadBadgeArcadeParts()
	if err != nil {
		return nil, err
	}
	c, err := parts.template(l.Template)
	if err != nil {
		return nil, err
	}
	if err := badgearcade.ApplyLayout(c, l); err != nil {
		return nil, err
	}
	out := image.NewNRGBA(image.Rect(0, 0, badgearcade.StageScreenW, badgearcade.StageScreenH))
	if stage != nil {
		draw.Draw(out, out.Bounds(), badgearcade.CoverImage(stage, badgearcade.StageScreenW, badgearcade.StageScreenH), image.Point{}, draw.Src)
	} else {
		bare := *c
		bare.MachinePrizes, bare.MachineFixed, bare.MachineAttachments = nil, nil, nil
		out = badgearcade.RenderMachine(&bare, parts.lookup)
	}
	// Obstacles and attachments (badges have no files here, so they're skipped).
	for _, o := range badgearcade.MachineObjects(c, parts.lookup) {
		p := o.Placement
		badgearcade.DrawPlaced(out, o.Image, float64(p.X), float64(p.Y), float64(p.ScaleW)*o.BaseScale, float64(p.ScaleH)*o.BaseScale, float64(p.Rotation))
	}
	if spot == nil {
		spot = badgeSpotPlaceholder
	}
	sprite := badgearcade.FitImage(spot, 128, 128, color.Transparent)
	for _, p := range c.MachinePrizes {
		badgearcade.DrawPlaced(out, sprite, float64(p.X), float64(p.Y), float64(p.ScaleW), float64(p.ScaleH), float64(p.Rotation))
	}
	return out, nil
}

// ownCreationArt returns the art of one of the player's creations of a kind.
func ownCreationArt(pid, id int64, kind string) (image.Image, bool) {
	if id <= 0 {
		return nil, false
	}
	var art []byte
	if db.QueryRow(`SELECT art FROM badge_arcade_creations WHERE id = $1 AND pid = $2 AND kind = $3`, id, pid, kind).Scan(&art) != nil {
		return nil, false
	}
	img, _, err := image.Decode(bytes.NewReader(art))
	return img, err == nil
}

// renderLayoutSpec renders a saved/previewed layout spec for a player.
func renderLayoutSpec(pid int64, spec badgeEditorLayoutSpec) (*image.NRGBA, error) {
	var stage, spot image.Image
	if img, ok := ownCreationArt(pid, spec.Stage, "stage"); ok {
		stage = img
	}
	if img, ok := ownCreationArt(pid, spec.Badge, "badge"); ok {
		spot = img
	}
	return renderLayout(spec.Layout, stage, spot)
}

func registerBadgeEditorLayout() {
	http.HandleFunc(badgeEditorBase+"api/layout/templates", badgeEditorLayoutTemplates)
	http.HandleFunc(badgeEditorBase+"api/layout/template", badgeEditorLayoutTemplate)
	http.HandleFunc(badgeEditorBase+"api/layout/obstacles", badgeEditorLayoutObstacles)
	http.HandleFunc(badgeEditorBase+"api/layout/part/", badgeEditorLayoutPart)
	http.HandleFunc(badgeEditorBase+"api/preview-layout", badgeEditorLayoutPreview)
	// Load the parts in the background so the first editor visit is quick.
	go loadBadgeArcadeParts()
}

func badgeEditorLayoutTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := badgeArcadeDeployTemplates()
	if err != nil {
		badgeEditorError(w, http.StatusServiceUnavailable, "template machines unavailable")
		return
	}
	type item struct {
		Name      string `json:"name"`
		Icon      string `json:"icon"`
		Stage     string `json:"stage"`
		Type      uint32 `json:"type"`
		Spots     int    `json:"spots"`
		Obstacles int    `json:"obstacles"`
		Difficult bool   `json:"difficult"`
	}
	out := []item{}
	for _, t := range templates {
		out = append(out, item{t.Name, t.Icon, t.Stage, t.Type, t.PrizeSlots, len(t.FixedObjects), t.Difficult})
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	badgeEditorJSON(w, http.StatusOK, out)
}

func badgeEditorLayoutTemplate(w http.ResponseWriter, r *http.Request) {
	parts, err := loadBadgeArcadeParts()
	if err != nil {
		badgeEditorError(w, http.StatusServiceUnavailable, "machine parts unavailable")
		return
	}
	c, err := parts.template(r.URL.Query().Get("name"))
	if err != nil {
		badgeEditorError(w, http.StatusNotFound, err.Error())
		return
	}
	// Joints for display: the editor moves their anchors with body A.
	type joint struct {
		KindA string  `json:"kindA"`
		A     uint32  `json:"a"`
		KindB string  `json:"kindB"`
		B     uint32  `json:"b"`
		X     float32 `json:"x"`
		Y     float32 `json:"y"`
	}
	kind := func(k uint32) string {
		if k == badgearcade.JointAttachment {
			return badgearcade.LayoutAttachment
		}
		return badgearcade.LayoutBadge
	}
	joints := []joint{}
	for _, j := range c.Joints {
		joints = append(joints, joint{kind(j.KindA), j.A, kind(j.KindB), j.B, j.X, j.Y})
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	badgeEditorJSON(w, http.StatusOK, map[string]any{
		"layout": badgearcade.LayoutFromMachine(c), "stage": c.Crane, "icon": c.Icon, "joints": joints, "type": c.Type,
	})
}

func badgeEditorLayoutObstacles(w http.ResponseWriter, r *http.Request) {
	parts, err := loadBadgeArcadeParts()
	if err != nil {
		badgeEditorError(w, http.StatusServiceUnavailable, "machine parts unavailable")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	badgeEditorJSON(w, http.StatusOK, map[string]any{
		"obstacles": parts.obstacles, "attachments": parts.attachments,
		"limits": map[string]any{
			"badges": badgearcade.LayoutMaxBadges, "obstacles": badgearcade.LayoutMaxObstacles, "attachments": badgearcade.LayoutMaxAttachments,
			"x": badgearcade.LayoutXRange, "y": badgearcade.LayoutYRange, "scale": badgearcade.LayoutScaleRange,
			"friction": badgearcade.LayoutFrictionRange, "bounce": badgearcade.LayoutBounceRange,
			"strength": [2]float32{badgearcade.LayoutStrengthRange[0], badgearcade.LayoutStrengthComfort},
		},
	})
}

func badgeEditorLayoutPart(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, badgeEditorBase+"api/layout/part/")
	kind, file, ok := strings.Cut(rest, "/")
	if !ok || !strings.HasSuffix(file, ".png") {
		http.NotFound(w, r)
		return
	}
	parts, err := loadBadgeArcadeParts()
	if err != nil {
		http.Error(w, "machine parts unavailable", http.StatusServiceUnavailable)
		return
	}
	b, err := parts.partPNG(kind, strings.TrimSuffix(file, ".png"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}

func badgeEditorLayoutPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		badgeEditorError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if badgeEditorRateLimited("preview:"+badgeEditorClientIP(r), 60, time.Minute) {
		badgeEditorError(w, http.StatusTooManyRequests, "too many previews, slow down a little")
		return
	}
	var spec badgeEditorLayoutSpec
	if !badgeEditorReadJSON(w, r, &spec) {
		return
	}
	if err := validateLayout(&spec.Layout); err != nil {
		badgeEditorError(w, http.StatusBadRequest, err.Error())
		return
	}
	pid, _ := mySessionPID(r) // own stage/badge only when logged in
	img, err := renderLayoutSpec(pid, spec)
	if err != nil {
		badgeEditorError(w, http.StatusBadRequest, err.Error())
		return
	}
	badgeEditorJSON(w, http.StatusOK, map[string]any{"image": dataURL(pngBytes(img))})
}

// parseLayoutSpec decodes and validates a saved layout spec.
func parseLayoutSpec(raw json.RawMessage) (badgeEditorLayoutSpec, error) {
	var spec badgeEditorLayoutSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, errors.New("invalid layout")
	}
	return spec, validateLayout(&spec.Layout)
}

// layoutSpotCount is the number of badge spots in a layout spec (for listings).
func layoutSpotCount(spec badgeEditorLayoutSpec) int {
	n := 0
	for _, o := range spec.Layout.Objects {
		if o.Kind == badgearcade.LayoutBadge {
			n++
		}
	}
	return n
}
