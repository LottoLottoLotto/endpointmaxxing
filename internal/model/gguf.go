package model

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// GGUFHeader is the metadata substrate reads from a GGUF file.
type GGUFHeader struct {
	Version  uint32
	Arch     string
	Name     string
	FileType string
}

// GGUF metadata value types.
const (
	gU8 = iota
	gI8
	gU16
	gI16
	gU32
	gI32
	gF32
	gBool
	gString
	gArray
	gU64
	gI64
	gF64
)

var scalarSize = map[uint32]int64{gU8: 1, gI8: 1, gU16: 2, gI16: 2, gU32: 4, gI32: 4, gF32: 4, gBool: 1, gU64: 8, gI64: 8, gF64: 8}

// fileTypes maps general.file_type (llama_ftype) to its name.
var fileTypes = map[uint32]string{
	0: "F32", 1: "F16", 2: "Q4_0", 3: "Q4_1", 7: "Q8_0", 8: "Q5_0", 9: "Q5_1",
	10: "Q2_K", 11: "Q3_K_S", 12: "Q3_K_M", 13: "Q3_K_L", 14: "Q4_K_S", 15: "Q4_K_M",
	16: "Q5_K_S", 17: "Q5_K_M", 18: "Q6_K", 19: "IQ2_XXS", 20: "IQ2_XS", 21: "Q2_K_S",
	22: "IQ3_XS", 23: "IQ3_XXS", 24: "IQ1_S", 25: "IQ4_NL", 26: "IQ3_S", 27: "IQ3_M",
	28: "IQ2_S", 29: "IQ2_M", 30: "IQ4_XS", 31: "IQ1_M", 32: "BF16", 36: "TQ1_0",
	37: "TQ2_0", 38: "MXFP4",
}

// maxString bounds metadata strings so a corrupt file cannot force a huge allocation.
const maxString = 1 << 24

// ReadGGUFHeader reads the general.* metadata, stopping once it has what it needs.
func ReadGGUFHeader(path string) (*GGUFHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := &ggufReader{r: bufio.NewReaderSize(f, 1<<16)}

	var magic [4]byte
	if _, err := io.ReadFull(r.r, magic[:]); err != nil || string(magic[:]) != "GGUF" {
		return nil, fmt.Errorf("%s: not a GGUF file", path)
	}
	h := &GGUFHeader{Version: r.u32()}
	if h.Version < 2 {
		return nil, fmt.Errorf("%s: GGUF v%d unsupported (need v2+)", path, h.Version)
	}
	r.u64() // tensor count
	kvs := r.u64()
	ftype, haveFtype := uint32(0), false
	for i := uint64(0); i < kvs && r.err == nil; i++ {
		key := r.str()
		t := r.u32()
		switch {
		case key == "general.architecture" && t == gString:
			h.Arch = r.str()
		case key == "general.name" && t == gString:
			h.Name = r.str()
		case key == "general.file_type" && (t == gU32 || t == gI32):
			ftype, haveFtype = r.u32(), true
		default:
			r.skip(t)
		}
		if h.Arch != "" && h.Name != "" && haveFtype {
			break
		}
	}
	if r.err != nil && !errors.Is(r.err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, r.err)
	}
	if h.Arch == "" {
		return nil, fmt.Errorf("%s: GGUF has no general.architecture", path)
	}
	if haveFtype {
		if n, ok := fileTypes[ftype]; ok {
			h.FileType = n
		} else {
			h.FileType = fmt.Sprintf("ftype%d", ftype)
		}
	}
	return h, nil
}

type ggufReader struct {
	r   *bufio.Reader
	err error
}

func (g *ggufReader) read(n int64) []byte {
	if g.err != nil {
		return nil
	}
	b := make([]byte, n)
	_, g.err = io.ReadFull(g.r, b)
	return b
}

func (g *ggufReader) discard(n int64) {
	if g.err != nil {
		return
	}
	_, g.err = g.r.Discard(int(n))
}

func (g *ggufReader) u32() uint32 {
	b := g.read(4)
	if g.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (g *ggufReader) u64() uint64 {
	b := g.read(8)
	if g.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (g *ggufReader) strLen() int64 {
	n := g.u64()
	if g.err == nil && n > maxString {
		g.err = fmt.Errorf("metadata string of %d bytes", n)
	}
	return int64(n)
}

func (g *ggufReader) str() string { return string(g.read(g.strLen())) }

func (g *ggufReader) skip(t uint32) {
	if sz, ok := scalarSize[t]; ok {
		g.discard(sz)
		return
	}
	switch t {
	case gString:
		g.discard(g.strLen())
	case gArray:
		et, n := g.u32(), g.u64()
		if sz, ok := scalarSize[et]; ok {
			g.discard(sz * int64(n))
			return
		}
		for i := uint64(0); i < n && g.err == nil; i++ {
			g.skip(et)
		}
	default:
		if g.err == nil {
			g.err = fmt.Errorf("unknown metadata type %d", t)
		}
	}
}
