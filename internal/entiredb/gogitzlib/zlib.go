// Package gogitzlib registers a klauspost/compress-backed zlib provider with
// go-git's plugin registry, so go-git's internal compression and
// decompression go through the same zlib implementation we already use in
// our packfile read and write paths.
//
// Importing this package for its side effects (typically as a blank import
// from a binary's main package or a top-level test entry point) is enough
// to install the provider — there is no exported API.
package gogitzlib

import (
	"fmt"
	"io"

	kzlib "github.com/klauspost/compress/zlib"

	"github.com/go-git/go-git/v6/x/plugin"
)

type provider struct{}

func (provider) NewReader(r io.Reader) (plugin.ZlibReader, error) {
	zr, err := kzlib.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("klauspost zlib new reader: %w", err)
	}
	zlr, ok := zr.(plugin.ZlibReader)
	if !ok {
		return nil, fmt.Errorf("klauspost zlib reader %T does not implement plugin.ZlibReader", zr)
	}
	return zlr, nil
}

func (provider) NewWriter(w io.Writer) plugin.ZlibWriter {
	return kzlib.NewWriter(w)
}

// init registers the klauspost-backed provider so it is in effect before
// any go-git operation resolves the zlib plugin. Plugin registrations are
// frozen on first resolution, which makes init the only correct place.
//

func init() {
	if err := plugin.Register(plugin.Zlib(), func() plugin.ZlibProvider {
		return provider{}
	}); err != nil {
		panic(fmt.Errorf("gogitzlib: register klauspost provider: %w", err))
	}
}
