package society

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nexora48/nexora-api/internal/platform/database"
)

var (
	ErrBlockExists   = errors.New("block code already exists")
	ErrBlockNotFound = errors.New("block not found")
	ErrBlockNotEmpty = errors.New("block has residents or history")
	ErrUnitExists    = errors.New("unit number already exists in block")
	ErrUnitNotFound  = errors.New("unit not found")
	ErrUnitOccupied  = errors.New("unit has residents or history")
)

// occupied = has an owner/tenant who is active or suspended
const occupiedPred = `m.status IN ('active','suspended') AND m.role_key IN ('resident_owner','resident_tenant')`

type Block struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Code          string    `json:"code"`
	SortOrder     int       `json:"sort_order"`
	UnitCount     int       `json:"unit_count"`
	OccupiedCount int       `json:"occupied_count"`
}

type Unit struct {
	ID        uuid.UUID `json:"id"`
	BlockID   uuid.UUID `json:"block_id"`
	BlockCode string    `json:"block_code"`
	Floor     int       `json:"floor"`
	Number    string    `json:"number"`
	UnitType  string    `json:"unit_type"`
	AreaSqft  *int      `json:"area_sqft,omitempty"`
	Occupancy string    `json:"occupancy"` // vacant | self_occupied | rented
}

// ── Blocks ───────────────────────────────────────────────────────────────────

func (r *Repository) ListBlocks(ctx context.Context, societyID uuid.UUID) ([]Block, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT b.id, b.name, b.code, b.sort_order,
		  (SELECT count(*) FROM units u WHERE u.block_id = b.id),
		  (SELECT count(DISTINCT m.unit_id) FROM memberships m JOIN units u ON u.id = m.unit_id
		     WHERE u.block_id = b.id AND `+occupiedPred+`)
		FROM blocks b WHERE b.society_id = $1 ORDER BY b.sort_order, b.code`, societyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Block{}
	for rows.Next() {
		var b Block
		if err := rows.Scan(&b.ID, &b.Name, &b.Code, &b.SortOrder, &b.UnitCount, &b.OccupiedCount); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *Repository) CreateBlock(ctx context.Context, societyID uuid.UUID, name, code string) (*Block, error) {
	b := Block{Name: name, Code: code}
	err := r.pool.QueryRow(ctx, `
		INSERT INTO blocks (society_id, name, code, sort_order)
		VALUES ($1,$2,$3, COALESCE((SELECT max(sort_order)+1 FROM blocks WHERE society_id=$1),0))
		RETURNING id, sort_order`, societyID, name, code).Scan(&b.ID, &b.SortOrder)
	if database.IsUnique(err, "") {
		return nil, ErrBlockExists
	}
	return &b, err
}

func (r *Repository) UpdateBlock(ctx context.Context, societyID, id uuid.UUID, name, code string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE blocks SET name=$3, code=$4 WHERE id=$2 AND society_id=$1`, societyID, id, name, code)
	if database.IsUnique(err, "") {
		return ErrBlockExists
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrBlockNotFound
	}
	return err
}

func (r *Repository) DeleteBlock(ctx context.Context, societyID, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM blocks WHERE id=$2 AND society_id=$1`, societyID, id)
	if database.IsForeignKey(err) {
		return ErrBlockNotEmpty
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrBlockNotFound
	}
	return err
}

func (r *Repository) GetBlock(ctx context.Context, societyID, id uuid.UUID) (*Block, error) {
	var b Block
	err := r.pool.QueryRow(ctx, `SELECT id, name, code, sort_order FROM blocks WHERE id=$2 AND society_id=$1`, societyID, id).
		Scan(&b.ID, &b.Name, &b.Code, &b.SortOrder)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBlockNotFound
	}
	return &b, err
}

// ── Units ────────────────────────────────────────────────────────────────────

// BulkSpec describes floors × flats-per-floor generation.
type BulkSpec struct {
	Floors        int
	UnitsPerFloor int
	StartFloor    int
	UnitType      string
	NumberFormat  string // tokens: {block} {floor} {seq} {seq:02} {seq:03}
}

type BulkResult struct {
	Numbers []string `json:"numbers"`
	Created int      `json:"created"`
	Skipped []string `json:"skipped_existing"`
}

func formatNumber(format, block string, floor, seq int) string {
	if format == "" {
		format = "{block}{floor}{seq:02}"
	}
	rep := strings.NewReplacer("{block}", block, "{floor}", fmt.Sprint(floor),
		"{seq:03}", fmt.Sprintf("%03d", seq), "{seq:02}", fmt.Sprintf("%02d", seq), "{seq}", fmt.Sprint(seq))
	return rep.Replace(format)
}

// BulkCreateUnits generates units for a block. With dryRun nothing is written.
func (r *Repository) BulkCreateUnits(ctx context.Context, societyID uuid.UUID, block *Block, spec BulkSpec, dryRun bool) (*BulkResult, error) {
	res := &BulkResult{Numbers: []string{}, Skipped: []string{}}
	type item struct {
		floor  int
		number string
	}
	var items []item
	seen := map[string]bool{}
	for f := 0; f < spec.Floors; f++ {
		floor := spec.StartFloor + f
		for s := 1; s <= spec.UnitsPerFloor; s++ {
			n := formatNumber(spec.NumberFormat, block.Code, floor, s)
			if seen[n] {
				return nil, fmt.Errorf("number format produces duplicate %q", n)
			}
			seen[n] = true
			items = append(items, item{floor, n})
			res.Numbers = append(res.Numbers, n)
		}
	}
	existing := map[string]bool{}
	rows, err := r.pool.Query(ctx, `SELECT number FROM units WHERE society_id=$1 AND block_id=$2`, societyID, block.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		existing[n] = true
	}
	rows.Close()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, it := range items {
		if existing[it.number] {
			res.Skipped = append(res.Skipped, it.number)
			continue
		}
		if !dryRun {
			if _, err := tx.Exec(ctx, `INSERT INTO units (society_id, block_id, floor, number, unit_type) VALUES ($1,$2,$3,$4,$5)`,
				societyID, block.ID, it.floor, it.number, spec.UnitType); err != nil {
				return nil, err
			}
		}
		res.Created++
	}
	if dryRun {
		return res, nil
	}
	return res, tx.Commit(ctx)
}

// unitSelect computes occupancy from active/suspended owner+tenant memberships.
const unitSelect = `
	SELECT u.id, u.block_id, b.code, u.floor, u.number, u.unit_type, u.area_sqft,
	  CASE WHEN bool_or(m.role_key='resident_tenant') THEN 'rented'
	       WHEN bool_or(m.role_key='resident_owner')  THEN 'self_occupied'
	       ELSE 'vacant' END
	FROM units u
	JOIN blocks b ON b.id = u.block_id
	LEFT JOIN memberships m ON m.unit_id = u.id AND ` + occupiedPred

type UnitFilter struct {
	BlockID   *uuid.UUID
	Floor     *int
	Occupancy string
	Query     string
	Limit     int
	Offset    int
}

func (r *Repository) ListUnits(ctx context.Context, societyID uuid.UUID, f UnitFilter) ([]Unit, int, error) {
	args := []any{societyID}
	where := []string{"u.society_id = $1"}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if f.BlockID != nil {
		add("u.block_id = ?", *f.BlockID)
	}
	if f.Floor != nil {
		add("u.floor = ?", *f.Floor)
	}
	if f.Query != "" {
		add("u.number ILIKE ?", "%"+f.Query+"%")
	}
	having := ""
	switch f.Occupancy {
	case "vacant":
		having = `HAVING count(m.id)=0`
	case "rented":
		having = `HAVING bool_or(m.role_key='resident_tenant')`
	case "self_occupied":
		having = `HAVING bool_or(m.role_key='resident_owner') AND NOT bool_or(m.role_key='resident_tenant')`
	}
	base := unitSelect + ` WHERE ` + strings.Join(where, " AND ") + ` GROUP BY u.id, b.code ` + having
	var total int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM (`+base+`) t`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, f.Limit, f.Offset)
	rows, err := r.pool.Query(ctx, base+fmt.Sprintf(` ORDER BY b.code, u.floor, u.number LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Unit{}
	for rows.Next() {
		var u Unit
		if err := rows.Scan(&u.ID, &u.BlockID, &u.BlockCode, &u.Floor, &u.Number, &u.UnitType, &u.AreaSqft, &u.Occupancy); err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

func (r *Repository) GetUnit(ctx context.Context, societyID, id uuid.UUID) (*Unit, error) {
	var u Unit
	err := r.pool.QueryRow(ctx, unitSelect+` WHERE u.society_id=$1 AND u.id=$2 GROUP BY u.id, b.code`, societyID, id).
		Scan(&u.ID, &u.BlockID, &u.BlockCode, &u.Floor, &u.Number, &u.UnitType, &u.AreaSqft, &u.Occupancy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnitNotFound
	}
	return &u, err
}

func (r *Repository) CreateUnit(ctx context.Context, societyID, blockID uuid.UUID, floor int, number, unitType string, area *int) (*Unit, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO units (society_id, block_id, floor, number, unit_type, area_sqft)
		SELECT $1, b.id, $3, $4, $5, $6 FROM blocks b WHERE b.id=$2 AND b.society_id=$1
		RETURNING id`, societyID, blockID, floor, number, unitType, area).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBlockNotFound
	}
	if database.IsUnique(err, "") {
		return nil, ErrUnitExists
	}
	if err != nil {
		return nil, err
	}
	return r.GetUnit(ctx, societyID, id)
}

func (r *Repository) UpdateUnit(ctx context.Context, societyID, id uuid.UUID, floor int, number, unitType string, area *int) error {
	tag, err := r.pool.Exec(ctx, `UPDATE units SET floor=$3, number=$4, unit_type=$5, area_sqft=$6 WHERE id=$2 AND society_id=$1`,
		societyID, id, floor, number, unitType, area)
	if database.IsUnique(err, "") {
		return ErrUnitExists
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrUnitNotFound
	}
	return err
}

func (r *Repository) DeleteUnit(ctx context.Context, societyID, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM units WHERE id=$2 AND society_id=$1`, societyID, id)
	if database.IsForeignKey(err) {
		return ErrUnitOccupied
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrUnitNotFound
	}
	return err
}

// ── Overview (Apartments screen) ─────────────────────────────────────────────

type Flat struct {
	ID           uuid.UUID  `json:"id"`
	Number       string     `json:"number"`
	UnitType     string     `json:"unit_type"`
	Occupancy    string     `json:"occupancy"`
	Role         *string    `json:"role,omitempty"` // owner | tenant (who is shown)
	OccupantName *string    `json:"occupant_name,omitempty"`
	MembershipID *uuid.UUID `json:"membership_id,omitempty"`
	Since        *time.Time `json:"since,omitempty"`
	Vacant       bool       `json:"vacant"`
}

type FloorView struct {
	Floor     int    `json:"floor"`
	Label     string `json:"label"`
	FlatCount int    `json:"flat_count"`
	Flats     []Flat `json:"flats"`
}

type BlockView struct {
	ID            uuid.UUID   `json:"id"`
	Name          string      `json:"name"`
	Code          string      `json:"code"`
	TotalFlats    int         `json:"total_flats"`
	OccupiedFlats int         `json:"occupied_flats"`
	OccupiedPct   int         `json:"occupied_pct"`
	Floors        []FloorView `json:"floors"`
}

func floorLabel(f int) string {
	if f == 0 {
		return "Ground"
	}
	return fmt.Sprintf("Floor %d", f)
}

// Overview returns blocks → floors → flats with the visible occupant (tenant wins over owner).
func (r *Repository) Overview(ctx context.Context, societyID uuid.UUID) ([]BlockView, error) {
	blocks, err := r.ListBlocks(ctx, societyID)
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT u.id, u.block_id, u.floor, u.number, u.unit_type,
		       m.id, m.role_key, us.full_name, COALESCE(m.move_in_date::timestamptz, m.created_at), m.status
		FROM units u
		LEFT JOIN memberships m ON m.unit_id = u.id AND `+occupiedPred+`
		LEFT JOIN users us ON us.id = m.user_id
		WHERE u.society_id = $1
		ORDER BY u.floor, u.number`, societyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type key struct {
		block uuid.UUID
		floor int
	}
	flats := map[uuid.UUID]*Flat{} // by unit
	order := map[key][]uuid.UUID{} // unit ids per block+floor
	flatBlock := map[uuid.UUID]key{}
	for rows.Next() {
		var (
			uid, bid            uuid.UUID
			floor               int
			number, typ         string
			mid                 *uuid.UUID
			role, name, mstatus *string
			since               *time.Time
		)
		if err := rows.Scan(&uid, &bid, &floor, &number, &typ, &mid, &role, &name, &since, &mstatus); err != nil {
			return nil, err
		}
		f, ok := flats[uid]
		if !ok {
			f = &Flat{ID: uid, Number: number, UnitType: typ, Occupancy: "vacant", Vacant: true}
			flats[uid] = f
			k := key{bid, floor}
			order[k] = append(order[k], uid)
			flatBlock[uid] = k
		}
		if mid != nil && role != nil {
			short := strings.TrimPrefix(*role, "resident_")
			// tenant wins over owner for the displayed occupant
			if f.Role == nil || short == "tenant" {
				f.Role, f.OccupantName, f.MembershipID, f.Since = &short, name, mid, since
			}
			f.Vacant = false
			if short == "tenant" {
				f.Occupancy = "rented"
			} else if f.Occupancy != "rented" {
				f.Occupancy = "self_occupied"
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]BlockView, 0, len(blocks))
	for _, b := range blocks {
		bv := BlockView{ID: b.ID, Name: b.Name, Code: b.Code, TotalFlats: b.UnitCount, OccupiedFlats: b.OccupiedCount, Floors: []FloorView{}}
		if b.UnitCount > 0 {
			bv.OccupiedPct = b.OccupiedCount * 100 / b.UnitCount
		}
		var floors []int
		for k := range order {
			if k.block == b.ID {
				floors = append(floors, k.floor)
			}
		}
		sort.Ints(floors)
		for _, fl := range floors {
			fv := FloorView{Floor: fl, Label: floorLabel(fl), Flats: []Flat{}}
			for _, uid := range order[key{b.ID, fl}] {
				fv.Flats = append(fv.Flats, *flats[uid])
			}
			fv.FlatCount = len(fv.Flats)
			bv.Floors = append(bv.Floors, fv)
		}
		out = append(out, bv)
	}
	return out, nil
}

// ── Stats ────────────────────────────────────────────────────────────────────

type Stats struct {
	Blocks             int `json:"blocks"`
	TotalFlats         int `json:"total_flats"`
	OccupiedFlats      int `json:"occupied_flats"`
	Residents          int `json:"residents"`
	PendingResidents   int `json:"pending_residents"`
	SuspendedResidents int `json:"suspended_residents"`
}

func (r *Repository) Stats(ctx context.Context, societyID uuid.UUID) (*Stats, error) {
	var s Stats
	err := r.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM blocks WHERE society_id=$1),
		  (SELECT count(*) FROM units  WHERE society_id=$1),
		  (SELECT count(DISTINCT m.unit_id) FROM memberships m WHERE m.society_id=$1 AND `+occupiedPred+`),
		  (SELECT count(*) FROM memberships m WHERE m.society_id=$1 AND m.status='active'
		       AND m.role_key IN ('resident_owner','resident_tenant','family_member')),
		  (SELECT count(*) FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.society_id=$1 AND m.status='active'
		       AND u.last_login_at IS NULL AND m.role_key IN ('resident_owner','resident_tenant','family_member')),
		  (SELECT count(*) FROM memberships m WHERE m.society_id=$1 AND m.status='suspended'
		       AND m.role_key IN ('resident_owner','resident_tenant','family_member'))`, societyID).
		Scan(&s.Blocks, &s.TotalFlats, &s.OccupiedFlats, &s.Residents, &s.PendingResidents, &s.SuspendedResidents)
	return &s, err
}
