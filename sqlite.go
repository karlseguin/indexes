package indexes

import (
	"database/sql"
	"encoding/binary"
	"strings"

	_ "gopkg.in/mattn/go-sqlite3.v1"
)

var (
	encoder = binary.LittleEndian
)

type batcher struct {
	*sql.Stmt
	count int
}

func newBatcher(db *sql.DB, count int) (batcher, error) {
	statements := make([]string, count)
	for i := 0; i < count; i++ {
		statements[i] = "select summary, ? as s from resources where id = ?"
	}

	stmt, err := db.Prepare(strings.Join(statements, " union all "))
	if err != nil {
		return batcher{}, err
	}
	return batcher{stmt, count}, nil
}

type SqliteStorage struct {
	*sql.DB
	get       *sql.Stmt
	iIndex    *sql.Stmt
	uIndex    *sql.Stmt
	dIndex    *sql.Stmt
	iResource *sql.Stmt
	uResource *sql.Stmt
	dResource *sql.Stmt
	batchers  []batcher
}

func newSqliteStorage(path string) (*SqliteStorage, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}

	get, err := db.Prepare("select ifnull(details, summary) d, case when details is null then 0 else 1 end as detailed from resources where id = ?")
	if err != nil {
		return nil, err
	}
	iIndex, err := db.Prepare("insert into indexes (type, payload, id) values (?, ?, ?)")
	if err != nil {
		return nil, err
	}
	uIndex, err := db.Prepare("update indexes set type = ?, payload = ? where id = ?")
	if err != nil {
		return nil, err
	}
	dIndex, err := db.Prepare("delete from indexes where id = ?")
	if err != nil {
		return nil, err
	}

	iResource, err := db.Prepare("insert into resources (summary, details, eid, id) values (?, ?, ?, ?)")
	if err != nil {
		return nil, err
	}
	uResource, err := db.Prepare("update resources set summary = ?, details = ?, eid = ? where id = ?")
	if err != nil {
		return nil, err
	}
	dResource, err := db.Prepare("delete from resources where id = ?")
	if err != nil {
		return nil, err
	}

	sizes := []int{25, 20, 15, 10, 5, 4, 3, 2, 1}
	batchers := make([]batcher, len(sizes))
	for i, size := range sizes {
		batcher, err := newBatcher(db, size)
		if err != nil {
			db.Close()
			return nil, err
		}
		batchers[i] = batcher
	}

	return &SqliteStorage{
		DB:        db,
		get:       get,
		iIndex:    iIndex,
		uIndex:    uIndex,
		dIndex:    dIndex,
		iResource: iResource,
		uResource: uResource,
		dResource: dResource,
		batchers:  batchers,
	}, nil
}

func (s *SqliteStorage) Get(id Id) (payload []byte, detailed bool) {
	s.get.QueryRow(id).Scan(&payload, &detailed)
	return payload, detailed
}

func (s *SqliteStorage) Fill(ids []interface{}, payloads [][]byte) error {
	l := len(ids) / 2
	for true {
		var batcher batcher
		for _, batcher = range s.batchers {
			if batcher.count <= l {
				break
			}
		}
		count := batcher.count * 2
		rows, err := batcher.Query(ids[:count]...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var index int
			var summary []byte
			rows.Scan(&summary, &index)
			payloads[index] = summary
		}
		rows.Close()
		if l -= batcher.count; l == 0 {
			break
		}
		ids = ids[count:]
	}
	return nil
}

func (s *SqliteStorage) LoadNResources(n int) (map[Id][]byte, error) {
	m := make(map[Id][]byte, n)
	rows, err := s.DB.Query("select id, summary from resources order by random() limit ?", n)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int
		var summary []byte
		rows.Scan(&id, &summary)
		m[Id(id)] = summary
	}
	return m, nil
}

func (s *SqliteStorage) ListCount() uint32 {
	count := 0
	s.DB.QueryRow("select count(*) from indexes where type = 3").Scan(&count)
	return uint32(count)
}

func (s *SqliteStorage) SetCount() uint32 {
	count := 0
	s.DB.QueryRow("select count(*) from indexes where type = 2").Scan(&count)
	return uint32(count)
}

func (s *SqliteStorage) LoadIds(newOnly bool) (map[string]Id, error) {
	var count int
	err := s.DB.QueryRow("select count(*) from resources").Scan(&count)
	if err != nil {
		return nil, err
	}

	var payload []byte
	err = s.DB.QueryRow("select payload from indexes where id = 'ids'").Scan(&payload)
	if err != nil {
		return nil, err
	}
	return extractIdMap(payload, count), nil
}

func (s *SqliteStorage) EachSet(newOnly bool, f func(name string, ids []Id)) error {
	return s.each(newOnly, 2, f)
}

func (s *SqliteStorage) EachList(newOnly bool, f func(name string, ids []Id)) error {
	return s.each(newOnly, 3, f)
}

func (s *SqliteStorage) ClearNew() error {
	_, err := s.DB.Exec("truncate table updated")
	return err
}

func (s *SqliteStorage) each(newOnly bool, tpe int, f func(name string, ids []Id)) error {
	var indexes *sql.Rows
	var err error

	if newOnly {
		indexes, err = s.DB.Query("select id, payload from indexes where id in (select id from updated where type = ?)", tpe)
	} else {
		indexes, err = s.DB.Query("select id, payload from indexes where type = ?", tpe)
	}
	if err != nil {
		return err
	}
	defer indexes.Close()

	for indexes.Next() {
		var id string
		var blob []byte
		indexes.Scan(&id, &blob)

		f(id, extractIdsFromIndex(blob))
	}
	return nil
}

func (s *SqliteStorage) UpsertSet(id string, payload []byte) ([]Id, error) {
	return s.upsertIndex(id, 2, payload)
}

func (s *SqliteStorage) UpsertList(id string, payload []byte) ([]Id, error) {
	return s.upsertIndex(id, 3, payload)
}

func (s *SqliteStorage) UpsertResource(id Id, eid string, summary []byte, details []byte) error {
	return s.upsert(s.iResource, s.uResource, summary, details, eid, id)
}

func (s *SqliteStorage) RemoveList(id string) error {
	return s.RemoveSet(id)
}

func (s *SqliteStorage) RemoveSet(id string) error {
	_, err := s.dIndex.Exec(id)
	return err
}

func (s *SqliteStorage) RemoveResource(id Id) error {
	_, err := s.dResource.Exec(id)
	return err
}

func (s *SqliteStorage) UpdateIds(payload []byte, estimatedCount int) (map[string]Id, error) {
	if err := s.upsert(s.iIndex, s.uIndex, 1, payload, "ids"); err != nil {
		return nil, err
	}
	return extractIdMap(payload, estimatedCount), nil
}

func (s *SqliteStorage) upsertIndex(id string, tpe int, payload []byte) ([]Id, error) {
	if err := s.upsert(s.iIndex, s.uIndex, tpe, payload, id); err != nil {
		return nil, err
	}
	return extractIdsFromIndex(payload), nil
}

func (s *SqliteStorage) upsert(insert *sql.Stmt, update *sql.Stmt, arguments ...interface{}) error {
	tx, err := s.Begin()
	if err != nil {
		return err
	}
	insert, update = tx.Stmt(insert), tx.Stmt(update)

	result, err := update.Exec(arguments...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	_, err = insert.Exec(arguments...)
	return err
}

func (s *SqliteStorage) Close() error {
	return s.DB.Close()
}

func extractIdsFromIndex(blob []byte) []Id {
	ids := make([]Id, len(blob)/IdSize)
	for i := 0; i < len(blob); i += IdSize {
		ids[i/IdSize] = Id(encoder.Uint32(blob[i:]))
	}
	return ids
}

func extractIdMap(payload []byte, count int) map[string]Id {
	ids := make(map[string]Id, count)
	for len(payload) > 0 {
		l := int(payload[0])
		payload = payload[1:]
		id := string(payload[:l])
		payload = payload[l:]
		ids[id] = Id(encoder.Uint32(payload))
		payload = payload[IdSize:]
	}
	return ids
}
