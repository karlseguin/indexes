package garbage5

import (
	. "github.com/karlseguin/expect"
	"os"
	"testing"
)

const TMP_PATH = "/tmp/garbage5.db"

type DatabaseTests struct{}

func Test_Database(t *testing.T) {
	Expectify(new(DatabaseTests), t)
}

func (_ DatabaseTests) CreatesAList() {
	db := createDB()
	db.CreateList("test:list", "a-1", "b-2", "c-3")
	assertList(db, "test:list", "a-1", "b-2", "c-3")
	db.Close()
	// assertList(openDB(), "test:list", "a-1", "b-2", "c-3")
}

func createDB() *Database {
	os.Remove(TMP_PATH) //ignore failures
	return openDB()
}

func openDB() *Database {
	db, err := New(TMP_PATH)
	if err != nil {
		panic(err)
	}
	return db
}

func assertList(db *Database, name string, expected ...string) {
	list := db.List(name)
	Expect(list.Len()).To.Equal(len(expected))
	i := 0

	list.Each(func(id uint32) bool {
		Expect(id).To.Equal(db.Id(expected[i], false))
		i++
		return true
	})
}
