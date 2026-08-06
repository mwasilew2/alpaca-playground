package store_test

import (
	"testing"

	"github.com/mwasilew2/alpaca-playground/internal/store"
	"github.com/mwasilew2/alpaca-playground/internal/store/storetest"
)

func TestMemRepository_Contract(t *testing.T) {
	storetest.RunRepositoryContract(t, func(t *testing.T) store.Repository {
		return store.NewMemRepository()
	})
}
