package main

// Installable eShop titles for downloadable Revivetendo TV videos: every live
// video whose uploader made it downloadable (videos_download.go) gets its own
// 3DS title - the moflex player (eshop-video/player/, built from
// /home/nico/custom-tools/moflex-player) with that one video in its RomFS,
// which the player starts at once, plus info.txt (name and length) - with the video's name, an icon and a
// HOME Menu banner made from its thumbnail and banner. account-proxy sells
// them through its eShop app code (eshop_apps.go) on the "Downloadable
// videos" channel, crediting paid downloads to the uploader.
//
// No compiling: makerom and bannertool (/home/nico/custom-tools/dkp-local-bin)
// put the fixed player program together with the per-video RomFS and
// metadata. The first build is the base title (version 0), which then never
// changes. When anything that goes into a title changes later - the video,
// its pictures, name or uploader, or the player files - the next revision is
// built as an update title (0004000E, version 1024 + revision), so consoles
// that have the video get the change the way 3DS updates work. A hash of the
// inputs in apps/video-<id>.json says what is built. The check only needs S3
// ETags, so it runs on every catalog sync (an approval, a download option, an
// edit) and once at startup.
//
// The unique ids stay below 0xF8000: NS only applies updates to unique ids up
// to 0xF7FFF (or 0xFF000-0xFF3FF) without the Nimbus NS patch.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
)

const (
	// Title unique id videoTitleUIDBase + video id (base 00040000 0F4..00,
	// update 0004000E 0F4..00); store ids NSUID base + video id.
	videoTitleUIDBase         = 0xF4000
	videoTitleMaxID           = 0x3FFF // keeps the unique id at most 0xF7FFF
	videoTitleNSUIDBase       = 50010099200000
	videoTitleUpdateNSUIDBase = 50010099300000
	// Part of every build hash: bump it to rebuild every title (as an update)
	// after changing how titles are built.
	videoTitleBuildVersion = "2"
	// Older update CIAs kept for consoles that still have that version (the
	// eShop fetches the installed version's TMD).
	videoTitleKeepOldUpdates = 2
)

func videoTitleUID(id int64) int64 { return videoTitleUIDBase + id }

func videoTitleID(id int64) string {
	return fmt.Sprintf("00040000%08X", uint32(videoTitleUID(id))<<8)
}

func videoTitleIDName(id int64) string { return "video-" + strconv.FormatInt(id, 10) }
func videoTitleCIA(id int64) string    { return videoTitleIDName(id) + ".cia" }
func videoTitleUpdateCIA(id int64, rev int) string {
	return fmt.Sprintf("%s-update-v%d.cia", videoTitleIDName(id), 1024+rev)
}
func videoTitleIcon(id int64) string   { return "i/" + videoTitleIDName(id) + "-icon.jpg" }
func videoTitleBanner(id int64) string { return "i/" + videoTitleIDName(id) + "-banner.jpg" }

func videoPlayerDir() string { return filepath.Join(videoRoot, "player") }
func videoAppsDir() string   { return filepath.Join(videoRoot, "apps") }

func videoDkpTool(name string) string {
	dir := os.Getenv("VIDEO_DKP_BIN")
	if dir == "" {
		dir = "/home/nico/custom-tools/dkp-local-bin"
	}
	return filepath.Join(dir, name)
}

// videoTitleState is apps/video-<id>.json: what is built for a video.
type videoTitleState struct {
	TitleID    string    `json:"title_id"`
	Hash       string    `json:"hash"`     // inputs of the newest build
	Revision   int       `json:"revision"` // 0 = base only, n = update version 1024+n
	Update     string    `json:"update,omitempty"`
	OldUpdates []string  `json:"old_updates,omitempty"`
	BuiltAt    time.Time `json:"built_at"`
}

func videoTitleSidecar(id int64) string {
	return filepath.Join(videoAppsDir(), videoTitleIDName(id)+".json")
}

func videoTitleLoad(id int64) (videoTitleState, bool) {
	var s videoTitleState
	raw, err := os.ReadFile(videoTitleSidecar(id))
	if err != nil || json.Unmarshal(raw, &s) != nil {
		return s, false
	}
	// Built for another title id (an earlier unique id range): start over.
	if s.TitleID != videoTitleID(id) {
		return videoTitleState{}, false
	}
	if _, err := os.Stat(filepath.Join(videoAppsDir(), videoTitleCIA(id))); err != nil {
		return videoTitleState{}, false
	}
	return s, true
}

func videoTitleSave(id int64, s videoTitleState) error {
	raw, _ := json.MarshalIndent(s, "", "  ")
	tmp := videoTitleSidecar(id) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, videoTitleSidecar(id))
}

var videoTitleKick = make(chan struct{}, 1)

// videoTitlesKick asks the builder to check every title (never blocks).
func videoTitlesKick() {
	select {
	case videoTitleKick <- struct{}{}:
	default:
	}
}

func videoTitleWorker() {
	// videoCmd's Pdeathsig belongs to the thread that starts the process.
	runtime.LockOSThread()
	for range videoTitleKick {
		videoTitlesBuildAll()
	}
}

type videoTitleJob struct {
	id, pid               int64
	title, uploader, kind string // kind "2d" or "3d"
	seconds               int
}

func videoTitlesBuildAll() {
	rows, err := db.Query(`SELECT id, pid, title, stereo, seconds FROM eshop_videos
		WHERE status = 'live' AND download_mode IN ('free', 'paid') ORDER BY id`)
	if err != nil {
		log.Printf("video titles: %v", err)
		return
	}
	var jobs []videoTitleJob
	for rows.Next() {
		var j videoTitleJob
		var stereo string
		if rows.Scan(&j.id, &j.pid, &j.title, &stereo, &j.seconds) == nil {
			j.kind = map[bool]string{false: "2d", true: "3d"}[stereo != ""]
			j.uploader, _ = pnidForPID(j.pid)
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	built := 0
	for _, j := range jobs {
		if j.id > videoTitleMaxID {
			continue
		}
		hash, err := videoTitleHash(j)
		if err != nil {
			log.Printf("video titles: #%d: %v", j.id, err)
			continue
		}
		state, ok := videoTitleLoad(j.id)
		if ok && state.Hash == hash {
			continue
		}
		start := time.Now()
		if !ok {
			// First build: the base title.
			if err := videoTitleBuild(j, -1, filepath.Join(videoAppsDir(), videoTitleCIA(j.id))); err != nil {
				log.Printf("video titles: #%d build failed: %v", j.id, err)
				continue
			}
			state = videoTitleState{TitleID: videoTitleID(j.id), Hash: hash}
			log.Printf("video titles: #%d %q built as %s in %s", j.id, j.title, state.TitleID, time.Since(start).Round(time.Millisecond))
		} else {
			// A change: the next revision as an update.
			rev := state.Revision + 1
			if rev > 1023 {
				log.Printf("video titles: #%d has no update versions left", j.id)
				continue
			}
			name := videoTitleUpdateCIA(j.id, rev)
			if err := videoTitleBuild(j, rev, filepath.Join(videoAppsDir(), name)); err != nil {
				log.Printf("video titles: #%d update build failed: %v", j.id, err)
				continue
			}
			if state.Update != "" {
				state.OldUpdates = append(state.OldUpdates, state.Update)
			}
			for len(state.OldUpdates) > videoTitleKeepOldUpdates {
				os.Remove(filepath.Join(videoAppsDir(), state.OldUpdates[0]))
				state.OldUpdates = state.OldUpdates[1:]
			}
			state.Revision, state.Update, state.Hash = rev, name, hash
			log.Printf("video titles: #%d %q update v%d built in %s", j.id, j.title, 1024+rev, time.Since(start).Round(time.Millisecond))
		}
		state.BuiltAt = time.Now().UTC()
		if err := videoTitleSave(j.id, state); err != nil {
			log.Printf("video titles: #%d: %v", j.id, err)
			continue
		}
		built++
	}
	if built > 0 {
		if err := videoSyncCatalog(); err != nil {
			log.Printf("video titles: catalog sync: %v", err)
		}
	}
}

// videoFileStamp identifies a stored file's content without downloading it.
func videoFileStamp(id int64, name string) (string, error) {
	if st, err := os.Stat(filepath.Join(videoSubmissionDir(id), name)); err == nil {
		return fmt.Sprintf("local:%d:%d", st.Size(), st.ModTime().UnixNano()), nil
	}
	c := videoS3()
	if c == nil {
		return "", fmt.Errorf("%s: not on disk and no S3", name)
	}
	info, err := c.StatObject(context.Background(), videoS3Bucket, videoS3Key(id, name), minio.StatObjectOptions{})
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return fmt.Sprintf("s3:%s:%d", info.ETag, info.Size), nil
}

// videoFetchFile copies a stored file to dst.
func videoFetchFile(id int64, name, dst string) error {
	if src, err := os.Open(filepath.Join(videoSubmissionDir(id), name)); err == nil {
		defer src.Close()
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, src); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
	c := videoS3()
	if c == nil {
		return fmt.Errorf("%s: not on disk and no S3", name)
	}
	return c.FGetObject(context.Background(), videoS3Bucket, videoS3Key(id, name), dst, minio.GetObjectOptions{})
}

var videoPlayerFiles = []string{"moflex-player.elf", "video-title.rsf", "video-update.rsf", "chime.wav", "fix_update_cia.py"}

func videoTitleHash(j videoTitleJob) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "v%s\x00%d\x00%s\x00%s\x00%s\x00%d\x00", videoTitleBuildVersion, j.id, j.title, j.uploader, j.kind, j.seconds)
	for _, name := range []string{"video.moflex", "thumb.jpg", "banner.jpg"} {
		stamp, err := videoFileStamp(j.id, name)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s=%s\x00", name, stamp)
	}
	for _, name := range videoPlayerFiles {
		f, err := os.Open(filepath.Join(videoPlayerDir(), name))
		if err != nil {
			return "", err
		}
		io.Copy(h, f)
		f.Close()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// videoTitleBuild builds the base title (rev < 0) or update revision rev into
// out, and installs the eShop icon and banner.
func videoTitleBuild(j videoTitleJob, rev int, out string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	work, err := os.MkdirTemp(videoRoot, ".title-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	romfs := filepath.Join(work, "romfs")
	if err := os.Mkdir(romfs, 0o755); err != nil {
		return err
	}
	// The player plays romfs:/video-2d|3d.moflex at start (the name tells it
	// how the frames pair up).
	if err := videoFetchFile(j.id, "video.moflex", filepath.Join(romfs, "video-"+j.kind+".moflex")); err != nil {
		return err
	}
	// Name and length for the player's screen (moflex files carry no length).
	if err := os.WriteFile(filepath.Join(romfs, "info.txt"), []byte(fmt.Sprintf("%s\n%d\n", j.title, j.seconds)), 0o644); err != nil {
		return err
	}
	thumb, banner := filepath.Join(work, "thumb.jpg"), filepath.Join(work, "banner.jpg")
	if err := videoFetchFile(j.id, "thumb.jpg", thumb); err != nil {
		return err
	}
	if err := videoFetchFile(j.id, "banner.jpg", banner); err != nil {
		return err
	}

	// HOME Menu icon (48x48) and banner picture (256x128), and the eShop's
	// own icon (128x128) and banner (400x168) - 4:2:0 JPEGs, the only kind the
	// 3DS shows.
	ff := mobipegFFmpeg()
	fit := func(src string, w, h int, dst string, extra ...string) error {
		args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", src,
			"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d", w, h, w, h)}
		args = append(args, extra...)
		return videoRun(ctx, ff, append(args, dst)...)
	}
	icon48, banner256 := filepath.Join(work, "icon.png"), filepath.Join(work, "banner.png")
	shopIcon, shopBanner := filepath.Join(work, "shop-icon.jpg"), filepath.Join(work, "shop-banner.jpg")
	jpeg := []string{"-pix_fmt", "yuvj420p", "-q:v", "3"}
	for _, step := range []error{
		fit(thumb, 48, 48, icon48),
		fit(banner, 256, 128, banner256),
		fit(thumb, 128, 128, shopIcon, jpeg...),
		fit(banner, 400, 168, shopBanner, jpeg...),
	} {
		if step != nil {
			return step
		}
	}

	publisher := j.uploader
	if publisher == "" {
		publisher = "Revivetendo"
	}
	smdh, bnr := filepath.Join(work, "icon.smdh"), filepath.Join(work, "banner.bnr")
	if err := videoRun(ctx, videoDkpTool("bannertool"), "makesmdh", "-s", j.title, "-l", "Revivetendo TV video", "-p", publisher,
		"-i", icon48, "-o", smdh); err != nil {
		return err
	}
	if err := videoRun(ctx, videoDkpTool("bannertool"), "makebanner", "-i", banner256,
		"-a", filepath.Join(videoPlayerDir(), "chime.wav"), "-o", bnr); err != nil {
		return err
	}
	uid := videoTitleUID(j.id)
	rsf := "video-title.rsf"
	var version []string
	if rev >= 0 {
		// Update: Category Patch, version 1024 + rev (major 1).
		rsf = "video-update.rsf"
		version = []string{"-major", "1", "-minor", strconv.Itoa(rev >> 4), "-micro", strconv.Itoa(rev & 15)}
	}
	cia := filepath.Join(work, "title.cia")
	args := []string{"-f", "cia", "-target", "t", "-exefslogo", "-o", cia,
		"-elf", filepath.Join(videoPlayerDir(), "moflex-player.elf"), "-rsf", filepath.Join(videoPlayerDir(), rsf),
		"-icon", smdh, "-banner", bnr,
		"-DAPP_ROMFS=" + romfs, "-DAPP_TITLE=rvtvideo",
		fmt.Sprintf("-DAPP_PRODUCT_CODE=CTR-N-V%05X", uid), fmt.Sprintf("-DAPP_UNIQUE_ID=0x%X", uid)}
	if err := videoRun(ctx, videoDkpTool("makerom"), append(args, version...)...); err != nil {
		return err
	}
	if rev >= 0 {
		// The update's exheader must carry the base's program id, or the
		// system keeps running the base.
		fixed := filepath.Join(work, "update.cia")
		if err := videoRun(ctx, "python3", "-I", filepath.Join(videoPlayerDir(), "fix_update_cia.py"), cia, fixed); err != nil {
			return err
		}
		cia = fixed
	}

	media := filepath.Join(videoRoot, "media")
	for src, dst := range map[string]string{shopIcon: videoTitleIcon(j.id), shopBanner: videoTitleBanner(j.id)} {
		if err := os.Rename(src, filepath.Join(media, dst)); err != nil {
			return err
		}
	}
	return os.Rename(cia, out)
}

// videoTitleCatalogEntry is the catalog "apps" entry for a video's title, or
// nil while none is built.
func videoTitleCatalogEntry(id, pid int64, title, desc, uploader, mode string, price int) map[string]any {
	state, ok := videoTitleLoad(id)
	if !ok {
		return nil
	}
	publisher := uploader
	if publisher == "" {
		publisher = "Revivetendo"
	}
	if mode != "paid" {
		price = 0
	}
	e := map[string]any{
		"ns_uid": videoTitleNSUIDBase + id, "title_id": state.TitleID,
		"name": title, "description": desc, "publisher": publisher,
		"icon": videoTitleIcon(id), "banner": videoTitleBanner(id),
		"price": price, "cia": videoTitleCIA(id), "new": true,
		"owner_pid": pid, "video_id": id,
	}
	if state.Update != "" {
		old := state.OldUpdates
		if old == nil {
			old = []string{}
		}
		e["update"] = map[string]any{"ns_uid": videoTitleUpdateNSUIDBase + id, "cia": state.Update, "old_cias": old}
	}
	return e
}
