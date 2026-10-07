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
