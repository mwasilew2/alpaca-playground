package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/mwasilew2/alpaca-playground/internal/store"
	"github.com/mwasilew2/alpaca-playground/internal/store/sqlitestore"
	"github.com/mwasilew2/alpaca-playground/internal/store/storetest"
)

func TestSQLiteRepository_Contract(t *testing.T) {
	storetest.RunRepositoryContract(t, func(t *testing.T) store.Repository {
		r, err := sqlitestore.Open(filepath.Join(t.TempDir(), "cache.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return r
	})
}
