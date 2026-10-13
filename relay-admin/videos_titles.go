package main

// Installable eShop titles for downloadable Revivetendo TV videos: every live
// video whose uploader made it downloadable (videos_download.go) gets its own
// 3DS title - the moflex player (eshop-video/player/, built from
// /home/nico/custom-tools/moflex-player) with that one video in its RomFS,
// which the player starts at once - with the video's name, an icon and a
// HOME Menu banner made from its thumbnail and banner. account-proxy sells
// them through its eShop app code (eshop_apps.go) on the "Downloadable
// videos" channel, crediting paid downloads to the uploader.
//
// No compiling: makerom and bannertool (/home/nico/custom-tools/dkp-local-bin)
// put the fixed player program together with the per-video RomFS and
// metadata. A title is (re)built when it is missing or anything that goes into
// it changed - the video, its pictures, name or uploader, or the player
// files - which a hash in apps/video-<id>.json records. The check only needs
// S3 ETags, so it runs on every catalog sync (an approval, a download option,
// an edit) and once at startup.

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
	// Title unique id videoTitleUIDBase + video id (title 00040000 0FA..00);
	// store id videoTitleNSUIDBase + video id.
	videoTitleUIDBase   = 0xFA000
	videoTitleMaxID     = 0x4FFF // keeps the unique id below 0xFF000
	videoTitleNSUIDBase = 50010099200000
	// Part of every build hash: bump it to rebuild every title after changing
	// how titles are built.
	videoTitleBuildVersion = "1"
)

func videoTitleID(id int64) string {
	return fmt.Sprintf("00040000%08X", uint32(videoTitleUIDBase+id)<<8)
}

func videoTitleCIA(id int64) string { return "video-" + strconv.FormatInt(id, 10) + ".cia" }

func videoTitleIcon(id int64) string   { return "i/video-" + strconv.FormatInt(id, 10) + "-icon.jpg" }
func videoTitleBanner(id int64) string { return "i/video-" + strconv.FormatInt(id, 10) + "-banner.jpg" }

func videoPlayerDir() string { return filepath.Join(videoRoot, "player") }

func videoDkpTool(name string) string {
	dir := os.Getenv("VIDEO_DKP_BIN")
	if dir == "" {
		dir = "/home/nico/custom-tools/dkp-local-bin"
	}
	return filepath.Join(dir, name)
}

// videoTitleBuilt reports whether the title's CIA exists (what the catalog
// lists).
func videoTitleBuilt(id int64) bool {
	_, err := os.Stat(filepath.Join(videoRoot, "apps", videoTitleCIA(id)))
	return err == nil
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
}

func videoTitlesBuildAll() {
	rows, err := db.Query(`SELECT id, pid, title, stereo FROM eshop_videos
		WHERE status = 'live' AND download_mode IN ('free', 'paid') ORDER BY id`)
	if err != nil {
		log.Printf("video titles: %v", err)
		return
	}
	var jobs []videoTitleJob
	for rows.Next() {
		var j videoTitleJob
		var stereo string
		if rows.Scan(&j.id, &j.pid, &j.title, &stereo) == nil {
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
		if videoTitleBuilt(j.id) && videoTitleStoredHash(j.id) == hash {
			continue
		}
		start := time.Now()
		if err := videoTitleBuild(j, hash); err != nil {
			log.Printf("video titles: #%d build failed: %v", j.id, err)
			continue
		}
		log.Printf("video titles: #%d %q built as %s in %s", j.id, j.title, videoTitleID(j.id), time.Since(start).Round(time.Millisecond))
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

func videoTitleHash(j videoTitleJob) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "v%s\x00%d\x00%s\x00%s\x00%s\x00", videoTitleBuildVersion, j.id, j.title, j.uploader, j.kind)
	for _, name := range []string{"video.moflex", "thumb.jpg", "banner.jpg"} {
		stamp, err := videoFileStamp(j.id, name)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s=%s\x00", name, stamp)
	}
	for _, name := range []string{"moflex-player.elf", "video-title.rsf", "chime.wav"} {
		f, err := os.Open(filepath.Join(videoPlayerDir(), name))
		if err != nil {
			return "", err
		}
		io.Copy(h, f)
		f.Close()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func videoTitleSidecar(id int64) string {
	return filepath.Join(videoRoot, "apps", "video-"+strconv.FormatInt(id, 10)+".json")
}

func videoTitleStoredHash(id int64) string {
	raw, err := os.ReadFile(videoTitleSidecar(id))
	if err != nil {
		return ""
	}
	var s struct {
		Hash string `json:"hash"`
	}
	json.Unmarshal(raw, &s)
	return s.Hash
}

func videoTitleBuild(j videoTitleJob, hash string) error {
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
	uid := videoTitleUIDBase + j.id
	cia := filepath.Join(work, "title.cia")
	if err := videoRun(ctx, videoDkpTool("makerom"), "-f", "cia", "-target", "t", "-exefslogo", "-o", cia,
		"-elf", filepath.Join(videoPlayerDir(), "moflex-player.elf"), "-rsf", filepath.Join(videoPlayerDir(), "video-title.rsf"),
		"-icon", smdh, "-banner", bnr,
		"-DAPP_ROMFS="+romfs, "-DAPP_TITLE=rvtvideo",
		fmt.Sprintf("-DAPP_PRODUCT_CODE=CTR-N-V%05X", uid), fmt.Sprintf("-DAPP_UNIQUE_ID=0x%X", uid)); err != nil {
		return err
	}

	// Install: pictures first, then the CIA (what the catalog checks), then
	// the hash.
	media := filepath.Join(videoRoot, "media")
	for src, dst := range map[string]string{shopIcon: videoTitleIcon(j.id), shopBanner: videoTitleBanner(j.id)} {
		if err := os.Rename(src, filepath.Join(media, dst)); err != nil {
			return err
		}
	}
	if err := os.Rename(cia, filepath.Join(videoRoot, "apps", videoTitleCIA(j.id))); err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]any{"hash": hash, "title_id": videoTitleID(j.id), "built_at": time.Now().UTC()})
	return os.WriteFile(videoTitleSidecar(j.id), raw, 0o644)
}

// videoTitleCatalogEntry is the catalog "apps" entry for a built title.
func videoTitleCatalogEntry(id, pid int64, title, desc, uploader, mode string, price int) map[string]any {
	publisher := uploader
	if publisher == "" {
		publisher = "Revivetendo"
	}
	if mode != "paid" {
		price = 0
	}
	return map[string]any{
		"ns_uid": videoTitleNSUIDBase + id, "title_id": videoTitleID(id),
		"name": title, "description": desc, "publisher": publisher,
		"icon": videoTitleIcon(id), "banner": videoTitleBanner(id),
		"price": price, "cia": videoTitleCIA(id), "new": true,
		"owner_pid": pid, "video_id": id,
	}
}
