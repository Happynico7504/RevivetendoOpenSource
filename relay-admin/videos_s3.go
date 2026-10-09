package main

// Converted Revivetendo TV videos live in S3 (Exoscale, the same S3_*
// credentials as the Mii images) under videos/{id}/{file}, in their own
// private bucket: videos still in review must not be readable by URL, and
// olv-data is public. The consoles can't fetch from Exoscale themselves (the
// 3DS doesn't trust its certificate, and Inkay gives the Wii U eShop our CA
// only), so account-proxy streams the files to them (samurai.go), and only
// files the catalog lists.
//
// If storing in S3 fails, the upload is marked failed with videoFailStorage,
// which the uploader sees. Both servers still look on local disk first, which
// only matters for a setup without S3 configured.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const videoS3Bucket = "revivetendo-tv"

var (
	videoS3Once   sync.Once
	videoS3Client *minio.Client
)

func videoS3() *minio.Client {
	videoS3Once.Do(func() {
		endpoint, key := os.Getenv("S3_ENDPOINT"), os.Getenv("S3_ACCESS_KEY")
		if endpoint == "" || key == "" {
			log.Printf("videos: S3 not configured, keeping videos on local disk")
			return
		}
		c, err := minio.New(endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(key, os.Getenv("S3_SECRET_KEY"), ""),
			Region: os.Getenv("S3_REGION"),
			Secure: true,
		})
		if err != nil {
			log.Printf("videos: S3 client: %v", err)
			return
		}
		ctx := context.Background()
		if ok, err := c.BucketExists(ctx, videoS3Bucket); err != nil {
			log.Printf("videos: S3 bucket check: %v", err)
		} else if !ok {
			if err := c.MakeBucket(ctx, videoS3Bucket, minio.MakeBucketOptions{Region: os.Getenv("S3_REGION")}); err != nil {
				log.Printf("videos: create bucket %s: %v", videoS3Bucket, err)
				return
			}
			log.Printf("videos: created private bucket %s", videoS3Bucket)
		}
		videoS3Client = c
	})
	return videoS3Client
}

func videoS3Key(id int64, name string) string {
	return "videos/" + strconv.FormatInt(id, 10) + "/" + name
}

// videoStoreConverted moves a finished conversion to S3 and deletes the local
// copies. Without S3 configured (a dev setup) the files stay local; when S3 is
// configured but the upload fails, the caller marks the video failed so the
// uploader is told instead of the video silently living on this disk.
func videoStoreConverted(id int64) error {
	if os.Getenv("S3_ENDPOINT") == "" {
		return nil
	}
	c := videoS3()
	if c == nil {
		return fmt.Errorf("S3 client unavailable")
	}
	ctx := context.Background()
	dir := videoSubmissionDir(id)
	for name, ct := range videoFileTypes {
		if _, err := c.FPutObject(ctx, videoS3Bucket, videoS3Key(id, name), filepath.Join(dir, name), minio.PutObjectOptions{ContentType: ct}); err != nil {
			return fmt.Errorf("upload %s: %w", name, err)
		}
	}
	for name := range videoFileTypes {
		os.Remove(filepath.Join(dir, name))
	}
	os.Remove(dir)
	log.Printf("videos: #%d stored in S3 (%s/videos/%d/)", id, videoS3Bucket, id)
	return nil
}

// videoDeleteStored removes a video's files from S3 and local disk.
func videoDeleteStored(id int64) error {
	if c := videoS3(); c != nil {
		for name := range videoFileTypes {
			if err := c.RemoveObject(context.Background(), videoS3Bucket, videoS3Key(id, name), minio.RemoveObjectOptions{}); err != nil {
				return fmt.Errorf("S3 delete %s: %w", name, err)
			}
		}
	}
	return os.RemoveAll(videoSubmissionDir(id))
}

// serveVideoFile serves one converted file (owner and staff previews),
// from local disk if it is still there, else from S3.
func serveVideoFile(w http.ResponseWriter, r *http.Request, id int64, name string) {
	ct, ok := videoFileTypes[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=60")
	if f, err := os.Open(filepath.Join(videoSubmissionDir(id), name)); err == nil {
		defer f.Close()
		if st, err := f.Stat(); err == nil {
			w.Header().Set("Content-Type", ct)
			http.ServeContent(w, r, "", st.ModTime(), f)
			return
		}
	}
	c := videoS3()
	if c == nil {
		http.NotFound(w, r)
		return
	}
	obj, err := c.GetObject(r.Context(), videoS3Bucket, videoS3Key(id, name), minio.GetObjectOptions{})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer obj.Close()
	st, err := obj.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	http.ServeContent(w, r, "", st.LastModified, obj) // minio.Object seeks with ranged GETs
}
