package relayhub

import (
	"context"
	"database/sql"
	"errors"
)

// RedirectInfo is the non-secret part of a redirects row.
type RedirectInfo struct {
	ID           int    `json:"id"`
	Type         string `json:"type"`
	FromHost     string `json:"from_host"`
	ToHost       string `json:"to_host"`
	GameServerID string `json:"game_server_id"`
	Port         int    `json:"port"`
	AccessMode   string `json:"access_mode"`
}

// Source is the read-only view of the main's data that relays may query.
// Nothing secret (passwords, tokens, hashes) is reachable through it.
type Source interface {
	PNIDForPID(ctx context.Context, pid uint64) (string, error) // ErrNotFound if unknown
	PIDForPNID(ctx context.Context, pnid string) (uint64, error)
	Redirects(ctx context.Context) ([]RedirectInfo, error)
	IsBanned(ctx context.Context, pid uint64) (bool, error)
}

type PGSource struct{ DB *sql.DB }

func (p *PGSource) PNIDForPID(ctx context.Context, pid uint64) (string, error) {
	var s string
	err := p.DB.QueryRowContext(ctx, `SELECT pnid FROM pnid_cache WHERE pid = $1`, int64(pid)).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && s == "") {
		return "", ErrNotFound
	}
	return s, err
}

func (p *PGSource) PIDForPNID(ctx context.Context, pnid string) (uint64, error) {
	var pid int64
	err := p.DB.QueryRowContext(ctx, `SELECT pid FROM pnid_cache WHERE pnid = $1`, pnid).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && pid == 0) {
		return 0, ErrNotFound
	}
	return uint64(pid), err
}

func (p *PGSource) Redirects(ctx context.Context) ([]RedirectInfo, error) {
	rows, err := p.DB.QueryContext(ctx, `
		SELECT id, type, from_host, to_host, COALESCE(game_server_id,''), COALESCE(port,0), COALESCE(access_mode,'open')
		FROM redirects WHERE enabled = true ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RedirectInfo{}
	for rows.Next() {
		var r RedirectInfo
		if err := rows.Scan(&r.ID, &r.Type, &r.FromHost, &r.ToHost, &r.GameServerID, &r.Port, &r.AccessMode); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *PGSource) IsBanned(ctx context.Context, pid uint64) (bool, error) {
	var b bool
	err := p.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM banned_users WHERE pid = $1)`, int64(pid)).Scan(&b)
	return b, err
}
