package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PretendoNetwork/wiiu-chat/relay-admin/swapdoodlepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// swapdoodle-server's own gRPC (swapdoodle/grpc) - its only RPC turns the
// Note ID a player sees in-game when reporting a note into a 15-minute
// presigned S3 URL for that note's object. Port/API key come from the same
// swapdoodle/.env main() already reads (PN_SD_GRPC_SERVER_PORT,
// PN_SD_CONFIG_GRPC_API_KEY); always dialed on loopback.
var (
	swapdoodleGRPCPort   string
	swapdoodleGRPCAPIKey string

	swapdoodleGRPCOnce   sync.Once
	swapdoodleGRPCClient swapdoodlepb.SwapdoodleClient
	swapdoodleGRPCErr    error
)

func swapdoodleGRPC() (swapdoodlepb.SwapdoodleClient, error) {
	swapdoodleGRPCOnce.Do(func() {
		if swapdoodleGRPCPort == "" {
			swapdoodleGRPCErr = fmt.Errorf("PN_SD_GRPC_SERVER_PORT not set in swapdoodle/.env")
			return
		}
		conn, err := grpc.NewClient("127.0.0.1:"+swapdoodleGRPCPort, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			swapdoodleGRPCErr = err
			return
		}
		swapdoodleGRPCClient = swapdoodlepb.NewSwapdoodleClient(conn)
	})
	return swapdoodleGRPCClient, swapdoodleGRPCErr
}

// The object key swapdoodle-server presigns: objects/%020d_%010d.bin.
var swapdoodleObjectKeyRe = regexp.MustCompile(`objects/(\d{20})_(\d{10})\.bin`)

// swapdoodleLookupNoteID resolves an in-game report Note ID to its data_id
// (parsed back out of the presigned object key, since the RPC returns only
// the URL) plus the presigned download URL itself.
func swapdoodleLookupNoteID(noteID string) (int64, string, error) {
	client, err := swapdoodleGRPC()
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if swapdoodleGRPCAPIKey != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "X-API-Key", swapdoodleGRPCAPIKey)
	}
	resp, err := client.GetPresignedUrlForReportId(ctx, &swapdoodlepb.GetPresignedUrlForReportIdRequest{NoteId: noteID})
	if err != nil {
		return 0, "", err
	}
	u, err := url.Parse(resp.GetUrl())
	if err != nil {
		return 0, "", fmt.Errorf("bad presigned URL: %v", err)
	}
	m := swapdoodleObjectKeyRe.FindStringSubmatch(u.Path)
	if m == nil {
		return 0, "", fmt.Errorf("unexpected object key in presigned URL: %s", u.Path)
	}
	dataID, _ := strconv.ParseInt(m[1], 10, 64)
	return dataID, resp.GetUrl(), nil
}

// swapdoodleNoteIDForDataID is the inverse of the decode in swapdoodle-server's
// GetPresignedUrlForReportId (swapdoodle/grpc/get_presigned_url_for_report_id.go),
// so every note page can show the Note ID players see in-game. Layout before
// the XOR: bytes 0-2 = data_id bytes 0-2, byte 4 = data_id byte 3, byte 3 =
// checksum making the XOR of all 8 bytes equal the access-key byte.
func swapdoodleNoteIDForDataID(dataID int64) string {
	const charset = "0123456789BCDFGHJKLMNPRTVWXY"
	var key uint8
	for _, c := range "76f26496" { // globals.HPP_ACCESS_KEY
		key ^= uint8(c)
		key = key<<4 | key>>4
	}
	d := uint32(dataID)
	b := [8]byte{byte(d), byte(d >> 8), byte(d >> 16), 0, byte(d >> 24)}
	b[3] = key ^ b[0] ^ b[1] ^ b[2] ^ b[4]
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	v ^= 0xDEAD9ED5
	var out []byte
	for v > 0 {
		out = append([]byte{charset[v%28]}, out...)
		v /= 28
	}
	return string(out)
}

// adminSwapdoodleLookup handles the Note ID search box on the notes list:
// /admin/spotpass-3ds/lookup?note_id=XXXX -> that note's detail page.
func adminSwapdoodleLookup(w http.ResponseWriter, r *http.Request) {
	noteID := strings.ToUpper(strings.Join(strings.Fields(r.URL.Query().Get("note_id")), ""))
	noteID = strings.ReplaceAll(noteID, "-", "")
	if noteID == "" {
		http.Redirect(w, r, "/inkay/admin/spotpass-3ds/", http.StatusSeeOther)
		return
	}
	dataID, _, err := swapdoodleLookupNoteID(noteID)
	if err != nil {
		http.Redirect(w, r, "/inkay/admin/spotpass-3ds/?err="+url.QueryEscape("Note ID "+noteID+": "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/inkay/admin/spotpass-3ds/note/%d", dataID), http.StatusSeeOther)
}

// adminSwapdoodleRaw redirects to a fresh presigned URL for the raw note
// object: /admin/spotpass-3ds/raw?note_id=XXXX (linked from every note page).
func adminSwapdoodleRaw(w http.ResponseWriter, r *http.Request) {
	_, presigned, err := swapdoodleLookupNoteID(r.URL.Query().Get("note_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, presigned, http.StatusFound)
}
