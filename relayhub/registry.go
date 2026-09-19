package relayhub

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Relay is one registered regional relay.
type Relay struct {
	ID         string
	Name       string
	Region     string // key of RegionCatalog
	Host       string // hostname or IP the DNS server hands out
	HealthPort int    // TCP port probed by the DNS server (0 = no probing)
	PublicKey  ed25519.PublicKey
	Enabled    bool
	CreatedAt  time.Time
	LastSeen   *time.Time
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

func (r *Relay) Validate() error {
	if !idRe.MatchString(r.ID) {
		return fmt.Errorf("relay id %q must be 1-31 chars of a-z 0-9 -", r.ID)
	}
	if _, ok := RegionCatalog[r.Region]; !ok {
		return fmt.Errorf("unknown region %q (known: %v)", r.Region, RegionNames())
	}
	if r.Host == "" {
		return errors.New("host required")
	}
	if r.HealthPort < 0 || r.HealthPort > 65535 {
		return errors.New("bad health port")
	}
	if len(r.PublicKey) != ed25519.PublicKeySize {
		return errors.New("bad public key")
	}
	return nil
}

// Registry stores relays. RelayKey is what relaylink.Server calls per request.
type Registry interface {
	Add(ctx context.Context, r *Relay) error
	List(ctx context.Context) ([]*Relay, error)
	SetEnabled(ctx context.Context, id string, enabled bool) error
	Delete(ctx context.Context, id string) error
	Touch(ctx context.Context, id string) error
	Get(ctx context.Context, id string) (*Relay, error)
}

var ErrNotFound = errors.New("not found")

// KeyLookup adapts a Registry to relaylink.Server.RelayKey with a short cache
// (so revocation takes effect within TTL) and throttled last_seen updates.
type KeyLookup struct {
	Reg Registry
	TTL time.Duration // default 10s
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cachedKey
	seen  map[string]time.Time
}

type cachedKey struct {
	key ed25519.PublicKey
	ok  bool
	exp time.Time
}

func (k *KeyLookup) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *KeyLookup) Lookup(id string) (ed25519.PublicKey, bool) {
	ttl := k.TTL
	if ttl == 0 {
		ttl = 10 * time.Second
	}
	now := k.now()
	k.mu.Lock()
	if k.cache == nil {
		k.cache, k.seen = map[string]cachedKey{}, map[string]time.Time{}
	}
	if c, ok := k.cache[id]; ok && now.Before(c.exp) {
		k.mu.Unlock()
		return c.key, c.ok
	}
	k.mu.Unlock()

	r, err := k.Reg.Get(context.Background(), id)
	ok := err == nil && r != nil && r.Enabled
	var key ed25519.PublicKey
	if ok {
		key = r.PublicKey
	}
	k.mu.Lock()
	k.cache[id] = cachedKey{key: key, ok: ok, exp: now.Add(ttl)}
	touch := ok && now.Sub(k.seen[id]) > time.Minute
	if touch {
		k.seen[id] = now
	}
	k.mu.Unlock()
	if touch {
		go k.Reg.Touch(context.Background(), id)
	}
	return key, ok
}

// ---- in-memory registry (tests, dry runs) ---------------------------------

type MemRegistry struct {
	mu sync.Mutex
	m  map[string]*Relay
}

func NewMemRegistry() *MemRegistry { return &MemRegistry{m: map[string]*Relay{}} }

func (r *MemRegistry) Add(_ context.Context, x *Relay) error {
	if err := x.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[x.ID]; dup {
		return fmt.Errorf("relay %q already exists", x.ID)
	}
	cp := *x
	cp.CreatedAt = time.Now()
	r.m[x.ID] = &cp
	return nil
}
func (r *MemRegistry) List(context.Context) ([]*Relay, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Relay
	for _, x := range r.m {
		cp := *x
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (r *MemRegistry) Get(_ context.Context, id string) (*Relay, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x, ok := r.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *x
	return &cp, nil
}
func (r *MemRegistry) SetEnabled(_ context.Context, id string, en bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	x, ok := r.m[id]
	if !ok {
		return ErrNotFound
	}
	x.Enabled = en
	return nil
}
func (r *MemRegistry) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[id]; !ok {
		return ErrNotFound
	}
	delete(r.m, id)
	return nil
}
func (r *MemRegistry) Touch(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if x, ok := r.m[id]; ok {
		now := time.Now()
		x.LastSeen = &now
	}
	return nil
}

// ---- Postgres registry ------------------------------------------------------

const registrySchema = `
CREATE TABLE IF NOT EXISTS relays (
	id          TEXT        PRIMARY KEY,
	name        TEXT        NOT NULL DEFAULT '',
	region      TEXT        NOT NULL,
	host        TEXT        NOT NULL,
	health_port INTEGER     NOT NULL DEFAULT 0,
	public_key  BYTEA       NOT NULL,
	enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	last_seen   TIMESTAMPTZ
)`

type PGRegistry struct{ DB *sql.DB }

func (p *PGRegistry) Init(ctx context.Context) error {
	_, err := p.DB.ExecContext(ctx, registrySchema)
	return err
}

func (p *PGRegistry) Add(ctx context.Context, r *Relay) error {
	if err := r.Validate(); err != nil {
		return err
	}
	_, err := p.DB.ExecContext(ctx,
		`INSERT INTO relays (id, name, region, host, health_port, public_key, enabled) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		r.ID, r.Name, r.Region, r.Host, r.HealthPort, []byte(r.PublicKey), r.Enabled)
	return err
}

func scanRelay(sc interface{ Scan(...any) error }) (*Relay, error) {
	var r Relay
	var pk []byte
	var seen sql.NullTime
	if err := sc.Scan(&r.ID, &r.Name, &r.Region, &r.Host, &r.HealthPort, &pk, &r.Enabled, &r.CreatedAt, &seen); err != nil {
		return nil, err
	}
	r.PublicKey = ed25519.PublicKey(pk)
	if seen.Valid {
		t := seen.Time
		r.LastSeen = &t
	}
	return &r, nil
}

const relayCols = `id, name, region, host, health_port, public_key, enabled, created_at, last_seen`

func (p *PGRegistry) List(ctx context.Context) ([]*Relay, error) {
	rows, err := p.DB.QueryContext(ctx, `SELECT `+relayCols+` FROM relays ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Relay
	for rows.Next() {
		r, err := scanRelay(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *PGRegistry) Get(ctx context.Context, id string) (*Relay, error) {
	r, err := scanRelay(p.DB.QueryRowContext(ctx, `SELECT `+relayCols+` FROM relays WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func (p *PGRegistry) exec(ctx context.Context, q string, args ...any) error {
	res, err := p.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PGRegistry) SetEnabled(ctx context.Context, id string, en bool) error {
	return p.exec(ctx, `UPDATE relays SET enabled=$2 WHERE id=$1`, id, en)
}
func (p *PGRegistry) Delete(ctx context.Context, id string) error {
	return p.exec(ctx, `DELETE FROM relays WHERE id=$1`, id)
}
func (p *PGRegistry) Touch(ctx context.Context, id string) error {
	_, err := p.DB.ExecContext(ctx, `UPDATE relays SET last_seen=NOW() WHERE id=$1`, id)
	return err
}
