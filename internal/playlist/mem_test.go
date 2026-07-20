package playlist_test

import (
	"testing"

	"github.com/the-algovn/radio-service/internal/library"
	"github.com/the-algovn/radio-service/internal/playlist"
)

func TestMemStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) (playlist.Store, library.Library) {
		lib := library.NewMemLibrary()
		return playlist.NewMemStore(lib), lib
	})
}
