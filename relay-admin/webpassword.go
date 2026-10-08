package main

// A web password lives on the account's wii_devices row when it has ever
// connected a Wii U, otherwise on its n3ds_devices row (3DS-only accounts).
// wii_devices holds only real Wii U consoles.

func webPasswordHash(pnid string) string {
	var h string
	db.QueryRow(`SELECT COALESCE(NULLIF(w.web_password_hash,''), n.web_password_hash, '')
		FROM (SELECT $1::text AS u) x
		LEFT JOIN wii_devices w ON w.username = x.u
		LEFT JOIN n3ds_devices n ON n.username = x.u`, pnid).Scan(&h)
	return h
}

func setWebPasswordHash(pnid, hash string) error {
	res, err := db.Exec(`UPDATE wii_devices SET web_password_hash = $1 WHERE username = $2`, hash, pnid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = db.Exec(`UPDATE n3ds_devices SET web_password_hash = $1 WHERE username = $2`, hash, pnid)
	return err
}

// canonicalPNID returns the PNID as a console stored it (our rows are keyed by
// the console's spelling; looked up case-insensitively), or pnid unchanged when
// no console has logged in with it. Web logins with a different spelling are
// refused with pnidCaseMismatchMessage.
func canonicalPNID(pnid string) string {
	var u string
	if db.QueryRow(`SELECT username FROM wii_devices WHERE lower(username) = lower($1)
		UNION ALL SELECT username FROM n3ds_devices WHERE lower(username) = lower($1) LIMIT 1`, pnid).Scan(&u) == nil && u != "" {
		return u
	}
	return pnid
}

// pnidOnRevivetendo reports whether a Wii U or 3DS has ever logged in to
// Revivetendo with this PNID (case-insensitive, like Nintendo's PNIDs).
// pnid_cache is not enough: it also holds Pretendo-only friends of our players.
func pnidOnRevivetendo(pnid string) bool {
	var ok bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM wii_devices WHERE lower(username) = lower($1))
		OR EXISTS(SELECT 1 FROM n3ds_devices WHERE lower(username) = lower($1))`, pnid).Scan(&ok)
	return ok
}
