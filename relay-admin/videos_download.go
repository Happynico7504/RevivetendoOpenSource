package main

// Download option for Revivetendo TV videos: the uploader decides per video
// whether it can be downloaded to the 3DS (as an installable title through the
// eShop, see account-proxy/eshop_apps.go) - not at all, for free, or for a
// price in RevivetendoCoins that is credited to their own wallet. Charging
// needs a wallet, which a player gets the first time they open a coin shop
// (eShop or Badge Arcade) on the network.
//
//	POST /my/videos/api/download   {id, mode: off|free|paid, price}

import (
	"encoding/json"
	"log"
	"net/http"
)

const videoMaxDownloadPrice = 500

func videoHasWallet(pid int64) bool {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM coin_wallets WHERE pid = $1`, pid).Scan(&n)
	return n > 0
}

func myVideosDownload(w http.ResponseWriter, r *http.Request) {
	pid, ok := videoAPIAuth(w, r, http.MethodPost)
	if !ok {
		return
	}
	var req struct {
		ID    int64  `json:"id"`
		Mode  string `json:"mode"`
		Price int    `json:"price"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req) != nil {
		videoError(w, http.StatusBadRequest, "vid.err_generic", "invalid request")
		return
	}
	switch req.Mode {
	case "off", "free":
		req.Price = 0
	case "paid":
		if !videoHasWallet(pid) {
			videoError(w, http.StatusForbidden, "vid.dl_err_wallet", "You need a RevivetendoCoin wallet to charge coins. Open the eShop on your 3DS once to get one.")
			return
		}
		if req.Price < 1 || req.Price > videoMaxDownloadPrice {
			videoError(w, http.StatusBadRequest, "vid.dl_err_price", "The price must be between 1 and 500 coins.")
			return
		}
	default:
		videoError(w, http.StatusBadRequest, "vid.err_generic", "unknown download option")
		return
	}
	var status string
	if db.QueryRow(`SELECT status FROM eshop_videos WHERE id = $1 AND pid = $2`, req.ID, pid).Scan(&status) != nil {
		videoError(w, http.StatusNotFound, "vid.err_generic", "unknown video")
		return
	}
	if _, err := db.Exec(`UPDATE eshop_videos SET download_mode = $2, download_price = $3, updated_at = NOW() WHERE id = $1`, req.ID, req.Mode, req.Price); err != nil {
		videoError(w, http.StatusInternalServerError, "vid.err_generic", "Something went wrong, please try again.")
		return
	}
	if status == "live" {
		if err := videoSyncCatalog(); err != nil {
			log.Printf("videos: catalog sync after download change on #%d: %v", req.ID, err)
		}
	}
	log.Printf("videos: #%d download set to %s (%d coins) by PID %d", req.ID, req.Mode, req.Price, pid)
	videoJSON(w, http.StatusOK, map[string]any{"ok": true})
}
