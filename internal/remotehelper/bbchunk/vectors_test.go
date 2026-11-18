package bbchunk

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/jmwoliver/xet-go/xethash"
	"github.com/stretchr/testify/require"
)

// vector is a known answer for one blob. A field left empty is not checked.
// TestClientVectors checks the vectors that entiredb generates.
type vector struct {
	Name        string
	Input       vectorInput
	ChunkEnds   []int64
	ChunkHashes []string
	// Xorbs lists the hashes of the xorbs that chunkBlob fills.
	Xorbs    []string
	FileHash string
}

// vectorInput describes the bytes of a blob. Generator is "hex" (Hex),
// "splitmix64" (Size, Seed), "linear" (Size, A, B) or "constant" (Size,
// Byte).
type vectorInput struct {
	Generator string
	Hex       string
	Size      int
	Seed      uint64
	A         int
	B         int
	Byte      byte
}

func (in vectorInput) bytes() ([]byte, error) {
	switch in.Generator {
	case "hex":
		return hex.DecodeString(in.Hex)
	case "splitmix64":
		return splitMix64Bytes(in.Size, in.Seed), nil
	case "linear":
		return linearBytes(in.Size, in.A, in.B), nil
	case "constant":
		return bytes.Repeat([]byte{in.Byte}, in.Size), nil
	}
	return nil, fmt.Errorf("unknown generator %q", in.Generator)
}

// builtinVectors come from Hugging Face's reference code, through entiredb's
// internal/xet tests:
//
//   - xet-core 0054283b9ce00f4683a9b9fa21a40301acec81f6:
//     xet_data/src/deduplication/chunking.rs and
//     xet_core_structures/src/merklehash/aggregated_hashes.rs.
//   - huggingface.js fd699e716a1a785e27febe49696150a1c48a8a71:
//     packages/xetchunk-wasm/tests/index.test.ts.
//
// Do not regenerate them from xet-go. A mismatch means that chunks or hashes
// no longer match Xet. Each blob fits in one xorb, so its xorb hash is the
// hash of all its chunks.
var builtinVectors = []vector{
	{
		Name:  "splitmix64 seed 0",
		Input: vectorInput{Generator: "splitmix64", Size: 1_000_000},
		ChunkEnds: []int64{
			84493, 134421, 144853, 243318, 271793, 336457, 467529, 494581, 582000, 596735,
			616815, 653164, 678202, 724510, 815591, 827760, 958832, 991092, 1000000,
		},
		ChunkHashes: []string{
			"6eca1e7dadaf08cca5d82d318c800f07c2ddcec115a7e8627e5edd9605b94b8d",
			"624ea34d72a06e5d43a1b8dd10763f0d22f165992c397ed97563899b27fdd88a",
			"4411d2ec847c6e3f451a7451ff3933adb4f1c31587421b9f730b698a78313b47",
			"6342bde97433e29e0779ad33eb8040d986679040361b3cc3a06230fe60dd6c9b",
			"405253fcf15bba751adc4f507d3453273daff81ed4d8acd71c521aa1cbddc0b5",
			"4482374af7f8bebfdb5c5df0299f80128d6c58886ad7b218c562b1d74064e4cb",
			"80acc8d39c853b4b8a8c6ad7b63bf2ea68f62c2226b92f06349f92cc84d213cc",
			"a7076d7d343f711fb20fe6cd023248d8d051e8fe7d44172596cd5c7ea7edaf65",
			"44755217bbb4dadc81ea7695765230a34a2e6cb3b55f373f1de35aeba79ae92c",
			"001adc1d302d5f039278325dcbd5ec3b194f4794f1629d6f962f9f4bb78a7bff",
			"f8460a337c186f07c2e225bb287a1d3d3d686dc69d0828e99640f7d8852c5b90",
			"c9bc3da29025dc1ba562d303d815151d9a937367abb766ae842a165e8493d9fe",
			"5044339dfd65e8163bdfe642614a6be604b04d6aeacf222cf219ad287bfc5cf1",
			"163622db0fe0da93f2ef964eed4c485f3c7a9c312f8e8e8a312ab4bb8141f13e",
			"e1730534a858aa0258ad8904ef12b829b8a123a1c611250275c9ca9471e4c650",
			"1fbc6854f9185caba1e1f55393f41f83b895b18f9c99245c029025ca48f1e14b",
			"8a27b66ccf05b864b6bef6fb5c970fe894f73d2330e8e98fe7841dcdbd9e9576",
			"bc70a33e7a9ec820cac24b87023469a57bdae1bf91cc3961b95806c64a525221",
			"03e5b5f5a088269ec4b329f1e04debfac4cb54b9c0facf038f7e8e0f054be7e2",
		},
	},
	{
		Name:      "constant byte forces maximum-size chunks",
		Input:     vectorInput{Generator: "constant", Size: 1_000_000, Byte: 59},
		ChunkEnds: []int64{131072, 262144, 393216, 524288, 655360, 786432, 917504, 1000000},
	},
	{
		Name:     "linear 200000 bytes, 2 chunks",
		Input:    vectorInput{Generator: "linear", Size: 200_000, A: 7, B: 13},
		Xorbs:    []string{"f0755d96ee199d4f01e83da9fe2c38353de7fd38fb79a320a7d50be510026f74"},
		FileHash: "d88d9e0b760f32c90b7c6592b4cb85251c01bb797371e3c637087b96ac578723",
	},
	{
		Name:     "linear 1000000 bytes, 8 chunks",
		Input:    vectorInput{Generator: "linear", Size: 1_000_000, A: 13, B: 37},
		Xorbs:    []string{"7be289d851485c40dcde52dbcf37a7823a5ef0bfcdf1ca1c5a32ad9594c71571"},
		FileHash: "041a50f3639f4a080a89237025d643e2a3e22935ca5d7dbc1a38f5c1e859f859",
	},
}

func TestVectors(t *testing.T) {
	t.Parallel()
	for _, v := range builtinVectors {
		t.Run(v.Name, func(t *testing.T) {
			t.Parallel()
			data, err := v.Input.bytes()
			require.NoError(t, err)

			chunks := splitAll(t, bytes.NewReader(data))
			if v.ChunkEnds != nil {
				ends := make([]int64, len(chunks))
				var end int64
				for i, c := range chunks {
					end += int64(len(c.Data))
					ends[i] = end
				}
				require.Equal(t, v.ChunkEnds, ends)
			}
			if v.ChunkHashes != nil {
				hashes := make([]string, len(chunks))
				for i, c := range chunks {
					hashes[i] = c.Hash.String()
				}
				require.Equal(t, v.ChunkHashes, hashes)
			}

			recipe, xorbs := chunkBlob(t, data)
			if v.Xorbs != nil {
				require.Equal(t, v.Xorbs, hashStrings(recipe.Xorbs))
				require.Equal(t, v.Xorbs, hashStrings(xorbHashes(xorbs)))
			}
			if v.FileHash != "" {
				require.Equal(t, v.FileHash, recipe.FileHash.String())
			}
		})
	}
}

func hashStrings(hs []xethash.Hash) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.String()
	}
	return out
}

// splitMix64Bytes is create_random_data from xet-core's chunking tests.
func splitMix64Bytes(n int, seed uint64) []byte {
	out := make([]byte, 0, n+7)
	state := seed
	for len(out) < n {
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		for range 8 {
			out = append(out, byte(z))
			z >>= 8
		}
	}
	return out[:n]
}

// xorshift32Bytes is the generator in huggingface.js's minimum chunk size
// regression test.
func xorshift32Bytes(n int, seed uint32) []byte {
	out := make([]byte, n)
	state := seed
	for i := range out {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		out[i] = byte(state)
	}
	return out
}

// linearBytes is makeData from huggingface.js's hash tests.
func linearBytes(n, a, b int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*a + b)
	}
	return out
}
