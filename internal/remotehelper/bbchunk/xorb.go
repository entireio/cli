package bbchunk

import (
	"fmt"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/jmwoliver/xet-go/xorb"
)

// Xorb is a sealed xorb.
//
// A push builds xorbs with xorb.Builder. It adds each chunk with
// AddHashed, the hash from Split and xorb.Auto, as the server does. So
// Network equals the body that xorb.Encode makes for the same chunks with
// xorb.Auto.
type Xorb struct {
	Hash xethash.Hash
	// Network is the xorb in xet-go's network format: the chunks with no
	// footer. The upload endpoint takes these bytes. The server adds the
	// footer when it stores the xorb.
	Network []byte
	// Footer is the chunk table of the xorb: the V1 footer that the server
	// adds, with a zero nonce. Footer.XorbHash is Hash.
	Footer *Footer
}

// Seal seals the xorb in b and empties b. The xorb owns its memory. Seal
// fails on an empty builder.
func Seal(b *xorb.Builder) (*Xorb, error) {
	body, f, err := b.Seal()
	if err != nil {
		return nil, fmt.Errorf("bbchunk: %w", err)
	}
	return &Xorb{Hash: f.XorbHash, Network: body, Footer: (*Footer)(f)}, nil
}

// Complete returns the complete form of x: its network form and its footer,
// as entiredb stores it.
func (x *Xorb) Complete() []byte {
	out := make([]byte, 0, len(x.Network)+xorb.FooterSize(x.Footer.Chunks()))
	return xorb.AppendFooter(append(out, x.Network...), (*xorb.Footer)(x.Footer))
}
